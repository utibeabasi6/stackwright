// Package fixtures holds the sample project's code. A run on a sample
// project with no upload runs this code.
package fixtures

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"embed"
)

//go:embed sample
var files embed.FS

// Sample returns the code as a .tar.gz, with the files under sample/.
func Sample() ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.AddFS(files); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
