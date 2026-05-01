package update

import (
	"fmt"
	"runtime"
)

// AssetName returns the goreleaser-produced archive name for the
// running platform and a given semver-shaped version string. The
// version is the bare semver ("1.2.3", no leading "v") because that's
// what goreleaser embeds in archive names.
//
// Mirrors the name_template in .goreleaser.yaml:
//
//	{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ x86_64 | arm | .Arch }}
//
// where amd64 is rewritten to x86_64 to match the Linux convention.
func AssetName(version string) string {
	return fmt.Sprintf("faultline_%s_%s_%s.tar.gz", version, runtime.GOOS, archLabel(runtime.GOARCH))
}

// archLabel maps a Go GOARCH value to the label goreleaser writes into
// archive names.
func archLabel(arch string) string {
	if arch == "amd64" {
		return "x86_64"
	}
	return arch
}

// ChecksumsName is the SHA256SUMS asset filename goreleaser publishes.
// Constant; goreleaser doesn't template the version into it.
const ChecksumsName = "SHA256SUMS"
