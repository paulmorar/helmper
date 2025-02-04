package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristofferNissen/helmper/internal/bootstrap"
	"github.com/ChristofferNissen/helmper/internal/output"
	"github.com/ChristofferNissen/helmper/pkg/copa"
	mySign "github.com/ChristofferNissen/helmper/pkg/cosign"
	"github.com/ChristofferNissen/helmper/pkg/helm"
	"github.com/ChristofferNissen/helmper/pkg/registry"
	"github.com/ChristofferNissen/helmper/pkg/trivy"
	"github.com/ChristofferNissen/helmper/pkg/util/state"
	"github.com/bobg/go-generics/slices"
	"github.com/k0kubun/go-ansi"
	"github.com/schollz/progressbar/v3"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func modify(cm *helm.ChartData, mirrorConfig []bootstrap.MirrorConfigSection) error {

	// modify images according to user specification
	for c, m := range *cm {
		for i, vs := range m {
			r, err := i.String()
			if err != nil {
				return err
			}

			if c.Images != nil {
				for _, e := range c.Images.Exclude {
					if strings.HasPrefix(r, e.Ref) {
						delete(m, i)
						slog.Info("excluded image", slog.String("image", r))
						break
					}
				}
				for _, ec := range c.Images.ExcludeCopacetic {
					if strings.HasPrefix(r, ec.Ref) {
						slog.Info("excluded image from copacetic patching", slog.String("image", r))
						f := false
						i.Patch = &f
						break
					}
				}
				for _, modify := range c.Images.Modify {
					if modify.From != "" {

						if strings.HasPrefix(r, modify.From) {
							delete(m, i)

							img, err := registry.RefToImage(
								strings.Replace(r, modify.From, modify.To, 1),
							)
							if err != nil {
								return err
							}

							img.Digest = i.Digest
							img.UseDigest = i.UseDigest
							img.Tag = i.Tag
							img.Patch = i.Patch

							m[&img] = vs

							newR, err := img.String()
							if err != nil {
								return err
							}
							slog.Info("modified image reference", slog.String("old_image", r), slog.String("new_image", newR))
						}
					}
				}
			}

			// Replace mirrors
			ms, err := slices.Filter(mirrorConfig, func(m bootstrap.MirrorConfigSection) (bool, error) {
				return m.Registry == i.Registry, nil
			})
			if err != nil {
				return err
			}

			if len(ms) > 0 {
				i.Registry = ms[0].Mirror
			}
		}
	}
	return nil
}

func Program(args []string) error {
	ctx := context.TODO()

	slogHandlerOpts := &slog.HandlerOptions{}
	if os.Getenv("HELMPER_LOG_LEVEL") == "DEBUG" {
		slogHandlerOpts.Level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, slogHandlerOpts))
	slog.SetDefault(logger)

	output.Header(version, commit, date)

	viper, err := bootstrap.LoadViperConfiguration(args)
	if err != nil {
		return err
	}
	var (
		k8sVersion   string                          = state.GetValue[string](viper, "k8s_version")
		verbose      bool                            = state.GetValue[bool](viper, "verbose")
		update       bool                            = state.GetValue[bool](viper, "update")
		all          bool                            = state.GetValue[bool](viper, "all")
		parserConfig bootstrap.ParserConfigSection   = state.GetValue[bootstrap.ParserConfigSection](viper, "parserConfig")
		importConfig bootstrap.ImportConfigSection   = state.GetValue[bootstrap.ImportConfigSection](viper, "importConfig")
		mirrorConfig []bootstrap.MirrorConfigSection = state.GetValue[[]bootstrap.MirrorConfigSection](viper, "mirrorConfig")
		registries   []registry.Registry             = state.GetValue[[]registry.Registry](viper, "registries")
		images       []registry.Image                = state.GetValue[[]registry.Image](viper, "images")
		charts       helm.ChartCollection            = state.GetValue[helm.ChartCollection](viper, "input")
		opts         []helm.Option                   = []helm.Option{
			helm.K8SVersion(k8sVersion),
			helm.Verbose(verbose),
			helm.Update(update),
		}
	)

	if verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	// Find input charts in configuration
	slog.Debug(
		"Found charts in config",
		slog.Int("count", len(charts.Charts)),
	)

	// STEP 1: Setup Helm
	charts, err = bootstrap.SetupHelm(
		&charts,
		opts...,
	)
	if err != nil {
		return err
	}
	// Output overview table of charts and subcharts
	go output.RenderChartTable(
		&charts,
		output.Update(update),
	)

	// STEP 2: Find images in Helm Charts and dependencies
	slog.Debug("Starting parsing user specified chart(s) for images..")
	co := helm.ChartOption{
		ChartCollection: &charts,
		IdentifyImages:  !parserConfig.DisableImageDetection,
		UseCustomValues: parserConfig.UseCustomValues,
	}
	chartImageHelmValuesMap, err := co.Run(
		ctx,
		opts...,
	)
	if err != nil {
		return err
	}

	err = modify(&chartImageHelmValuesMap, mirrorConfig)
	if err != nil {
		return err
	}

	// Add in images from config
	placeHolder := helm.Chart{
		Name:    "images",
		Version: "0.0.0",
	}
	m := map[*registry.Image][]string{}
	for _, i := range images {
		m[&i] = []string{}
	}
	chartImageHelmValuesMap[placeHolder] = m

	// Output table of image to helm chart value path
	go func() {
		output.RenderHelmValuePathToImageTable(chartImageHelmValuesMap)
		slog.Debug("Parsing of user specified chart(s) completed")
	}()

	// STEP 3: Validate and correct image references from charts
	slog.Debug("Checking presence of images from chart(s) in registries...")
	cs, imgs, err := helm.IdentifyImportCandidates(
		ctx,
		registries,
		chartImageHelmValuesMap,
		all,
	)
	if err != nil {
		return err
	}
	_ = output.RenderChartOverviewTable(
		ctx,
		viper,
		len(charts.Charts),
		registries,
		charts,
	)
	// Output table of image status in registries
	_ = output.RenderImageOverviewTable(
		ctx,
		viper,
		len(imgs),
		registries,
		chartImageHelmValuesMap,
	)
	slog.Debug("Finished checking image availability in registries")

	// Import charts to registries
	switch {
	case importConfig.Import.Enabled && len(cs.Charts) > 0:
		err := helm.ChartImportOption{
			Registries:      registries,
			ChartCollection: &cs,
			All:             all,
			ModifyRegistry:  importConfig.Import.ReplaceRegistryReferences,
		}.Run(ctx, opts...)
		if err != nil {
			return fmt.Errorf("internal: error importing chart to registry: %w", err)
		}

		if importConfig.Import.Cosign.Enabled {
			slog.Debug("Cosign enabled")
			signo := mySign.SignChartOption{
				ChartCollection: &cs,
				Registries:      registries,

				KeyRef:            importConfig.Import.Cosign.KeyRef,
				KeyRefPass:        *importConfig.Import.Cosign.KeyRefPass,
				AllowInsecure:     importConfig.Import.Cosign.AllowInsecure,
				AllowHTTPRegistry: importConfig.Import.Cosign.AllowHTTPRegistry,
			}
			if err := signo.Run(); err != nil {
				slog.Error("Error signing with Cosign")
				return err
			}
		}
	}

	switch {
	case importConfig.Import.Enabled && importConfig.Import.Copacetic.Enabled:
		slog.Debug("Import enabled and Copacetic enabled")
		patch := make([]*registry.Image, 0)
		push := make([]*registry.Image, 0)

		bar := progressbar.NewOptions(len(imgs), progressbar.OptionSetWriter(ansi.NewAnsiStdout()), // "github.com/k0kubun/go-ansi"
			progressbar.OptionEnableColorCodes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionOnCompletion(func() {
				fmt.Fprint(os.Stderr, "\n")
			}),
			progressbar.OptionSetRenderBlankState(true),
			progressbar.OptionSetWidth(15),
			progressbar.OptionSetDescription("Scanning images before patching...\r"),
			progressbar.OptionShowDescriptionAtLineEnd(),
			progressbar.OptionSetTheme(progressbar.Theme{
				Saucer:        "[green]=[reset]",
				SaucerHead:    "[green]>[reset]",
				SaucerPadding: " ",
				BarStart:      "[",
				BarEnd:        "]",
			}))

		so := trivy.ScanOption{
			DockerHost:    importConfig.Import.Copacetic.Buildkitd.Addr,
			TrivyServer:   importConfig.Import.Copacetic.Trivy.Addr,
			Insecure:      importConfig.Import.Copacetic.Trivy.Insecure,
			IgnoreUnfixed: importConfig.Import.Copacetic.Trivy.IgnoreUnfixed,
			Architecture:  importConfig.Import.Architecture,
		}

		for _, i := range imgs {

			if i.Patch != nil {
				if !*i.Patch {
					ref, err := i.String()
					if err != nil {
						return err
					}
					slog.Debug("image should not be patched",
						slog.String("image", ref))
					push = append(push, &i)
					continue
				}
			}

			ref, err := i.String()
			if err != nil {
				return err
			}
			r, err := so.Scan(ref)
			if err != nil {
				return err
			}

			switch copa.SupportedOS(r.Metadata.OS) {
			case true:
				// filter images with no os-pkgs as copa has nothing to do
				switch trivy.ContainsOsPkgs(r.Results) {
				case true:
					slog.Debug("Image does contain os-pkgs vulnerabilities",
						slog.String("image", ref))
					patch = append(patch, &i)
				case false:
					slog.Warn("Image does not contain os-pkgs. The image will not be patched.",
						slog.String("image", ref),
					)
					push = append(push, &i)
				}

			case false:
				slog.Warn("Image contains an unsupported OS. The image will not be patched.",
					slog.String("image", ref),
				)
				push = append(push, &i)
			}

			// Write report to filesystem
			name, _ := i.ImageName()
			fileName := fmt.Sprintf("%s:%s.json", name, i.Tag)
			fileName = filepath.Join(importConfig.Import.Copacetic.Output.Reports.Folder, "prescan-"+strings.ReplaceAll(fileName, "/", "-"))
			b, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(fileName, b, os.ModePerm); err != nil {
				return err
			}

			_ = bar.Add(1)
		}

		_ = bar.Finish()

		// determine fully qualified output path for images
		reportFilePaths := make(map[*registry.Image]string)
		reportPostFilePaths := make(map[*registry.Image]string)
		outFilePaths := make(map[*registry.Image]string)
		for _, i := range append(patch, push...) {
			name, _ := i.ImageName()
			fileName := fmt.Sprintf("prescan-%s:%s.json", name, i.Tag)
			reportFilePaths[i] = filepath.Join(
				importConfig.Import.Copacetic.Output.Reports.Folder,
				strings.ReplaceAll(fileName, "/", "-"),
			)
			fileName = fmt.Sprintf("postscan-%s:%s.json", name, i.Tag)
			reportPostFilePaths[i] = filepath.Join(
				importConfig.Import.Copacetic.Output.Reports.Folder,
				strings.ReplaceAll(fileName, "/", "-"),
			)
			out := fmt.Sprintf("%s:%s.tar", name, i.Tag)
			outFilePaths[i] = filepath.Join(
				importConfig.Import.Copacetic.Output.Tars.Folder,
				strings.ReplaceAll(out, "/", "-"),
			)
		}

		// Clean up files
		defer func() {
			if importConfig.Import.Copacetic.Output.Reports.Clean {
				for _, v := range reportFilePaths {
					_ = os.RemoveAll(v)
				}
				for _, v := range reportPostFilePaths {
					_ = os.RemoveAll(v)
				}
			}
			if importConfig.Import.Copacetic.Output.Reports.Clean {
				for _, v := range outFilePaths {
					_ = os.RemoveAll(v)
				}
			}
		}()

		// Import images without os-pkgs vulnerabilities
		iOpts := registry.ImportOption{
			Registries:   registries,
			Imgs:         push,
			All:          all,
			Architecture: importConfig.Import.Architecture,
		}
		err = iOpts.Run(ctx)
		if err != nil {
			return err
		}

		// Patch image and save to tar
		po := copa.PatchOption{
			Imgs:       patch,
			Registries: registries,
			Buildkit: struct {
				Addr       string
				CACertPath string
				CertPath   string
				KeyPath    string
			}{
				Addr:       importConfig.Import.Copacetic.Buildkitd.Addr,
				CACertPath: importConfig.Import.Copacetic.Buildkitd.CACertPath,
				CertPath:   importConfig.Import.Copacetic.Buildkitd.CertPath,
				KeyPath:    importConfig.Import.Copacetic.Buildkitd.KeyPath,
			},
			IgnoreErrors: importConfig.Import.Copacetic.IgnoreErrors,
			Architecture: importConfig.Import.Architecture,
		}
		err = po.Run(ctx, reportFilePaths, outFilePaths)
		if err != nil {
			return err
		}

		bar = progressbar.NewOptions(len(imgs), progressbar.OptionSetWriter(ansi.NewAnsiStdout()), // "github.com/k0kubun/go-ansi"
			progressbar.OptionEnableColorCodes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionOnCompletion(func() {
				fmt.Fprint(os.Stderr, "\n")
			}),
			progressbar.OptionSetRenderBlankState(true),
			progressbar.OptionSetWidth(15),
			progressbar.OptionSetDescription("Scanning images after patching...\r"),
			progressbar.OptionShowDescriptionAtLineEnd(),
			progressbar.OptionSetTheme(progressbar.Theme{
				Saucer:        "[green]=[reset]",
				SaucerHead:    "[green]>[reset]",
				SaucerPadding: " ",
				BarStart:      "[",
				BarEnd:        "]",
			}))
		err = func(out string, prefix string) error {
			for _, i := range imgs {
				ref, _ := i.String()
				r, err := so.Scan(ref)
				if err != nil {
					return err
				}

				// Write report to filesystem
				name, _ := i.ImageName()
				fileName := fmt.Sprintf("%s:%s.json", name, i.Tag)
				fileName = filepath.Join(out, prefix+strings.ReplaceAll(fileName, "/", "-"))
				b, err := json.MarshalIndent(r, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(fileName, b, os.ModePerm); err != nil {
					return err
				}

				_ = bar.Add(1)
			}
			return nil
		}(importConfig.Import.Copacetic.Output.Reports.Folder, "postscan-")
		if err != nil {
			return err
		}

		_ = bar.Finish()

		if importConfig.Import.Cosign.Enabled {
			signo := mySign.SignOption{
				Imgs:       append(patch, push...),
				Registries: registries,

				KeyRef:            importConfig.Import.Cosign.KeyRef,
				KeyRefPass:        *importConfig.Import.Cosign.KeyRefPass,
				AllowInsecure:     importConfig.Import.Cosign.AllowInsecure,
				AllowHTTPRegistry: importConfig.Import.Cosign.AllowHTTPRegistry,
			}
			if err := signo.Run(); err != nil {
				return err
			}
		}

	case importConfig.Import.Enabled:
		slog.Debug("Only import enabled")
		// convert to pointer array to enable mutable values
		imgPs := make([]*registry.Image, 0)
		for _, i := range imgs {
			imgPs = append(imgPs, &i)
		}

		err := registry.ImportOption{
			Registries:   registries,
			Imgs:         imgPs,
			All:          all,
			Architecture: importConfig.Import.Architecture,
		}.Run(ctx)
		if err != nil {
			return err
		}

		if importConfig.Import.Cosign.Enabled {
			signo := mySign.SignOption{
				Imgs:       imgPs,
				Registries: registries,

				KeyRef:            importConfig.Import.Cosign.KeyRef,
				KeyRefPass:        *importConfig.Import.Cosign.KeyRefPass,
				AllowInsecure:     importConfig.Import.Cosign.AllowInsecure,
				AllowHTTPRegistry: importConfig.Import.Cosign.AllowHTTPRegistry,
			}
			if err := signo.Run(); err != nil {
				return err
			}
		}
	}

	return nil
}
