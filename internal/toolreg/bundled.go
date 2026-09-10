package toolreg

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"

	"gopkg.in/yaml.v3"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

//go:embed bundled/*.yaml
var bundledFS embed.FS

// BundledMetadata is what this build knows about tools it did not write.
//
// It used to live at configs/tools/n9e.yaml and be loaded only if somebody
// set tools.metadata_dir at it. Nobody ever did, and the file was not in the
// container image at all — so six n9e tools ran with no produces, no side
// effect and no suites, which quietly degraded every path that scores or
// groups by them.
//
// Compiled in, for the same reason internal/tool's builtin metadata is: it is
// knowledge that ships with the product rather than a decision a deployment
// makes. Declaring a tool that is not connected costs nothing — the entry
// simply never matches — so a deployment without n9e is unaffected.
//
// A deployment's own declarations still go in tools.metadata_dir, and the
// console's go in the database. Three layers, in that order, each with a
// reason to exist:
//
//	bundled  what this build knows        in git, no deployment step
//	files    what this deployment knows   operator-owned, reviewable
//	database what the console changed     edited live, audited, overlay
func BundledMetadata() Source {
	return StaticSource{Label: "bundled", Metas: mustLoadBundled()}
}

// mustLoadBundled parses the embedded files at first use.
//
// A parse failure here is a build that shipped broken data, not a deployment
// problem, so it panics rather than degrading: the tests below run the same
// parse, so this cannot reach a release.
func mustLoadBundled() []ops.ToolMetadata {
	metas, err := loadBundled()
	if err != nil {
		panic("toolreg: bundled metadata is unreadable, which is a build error: " + err.Error())
	}
	return metas
}

func loadBundled() ([]ops.ToolMetadata, error) {
	entries, err := fs.ReadDir(bundledFS, "bundled")
	if err != nil {
		return nil, err
	}
	var out []ops.ToolMetadata
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := bundledFS.ReadFile(path.Join("bundled", e.Name()))
		if err != nil {
			return nil, err
		}
		var f metadataFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("bundled/%s: %w", e.Name(), err)
		}
		out = append(out, f.Tools...)
	}
	return out, nil
}

// bundledNames is only for tests and diagnostics.
func bundledNames(ctx context.Context) []string {
	metas, _ := BundledMetadata().Load(ctx)
	names := make([]string, 0, len(metas))
	for _, m := range metas {
		names = append(names, m.Name)
	}
	return names
}
