package path

import (
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// CopyDir copies the contents of a directory from 'src' to 'dest'
func CopyDir(src string, dest string) error {
	mode, err := os.Stat(src)
	if err != nil {
		return err
	}

	if mode.IsDir() {
		dirContents, err := os.ReadDir(src)
		if err != nil {
			return err
		}

		for _, f := range dirContents {
			srcFileaPath := filepath.Join(src, f.Name())
			destFilePath := filepath.Join(dest, f.Name())
			if err := CopyDir(srcFileaPath, destFilePath); err != nil {
				return err
			}
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), os.ModePerm); err != nil {
		return err
	}

	return copySingleFile(src, dest, mode)
}

func copySingleFile(src string, dest string, mode os.FileInfo) error {
	if src == dest {
		return nil
	}

	// Ensure the file is not a directory
	file, err := os.Stat(src)
	if err != nil {
		return err
	}
	if file.IsDir() {
		return fmt.Errorf("unable to copy directories: %s", src)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	destFile, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer destFile.Close()

	err = os.Chmod(destFile.Name(), mode.Mode())
	if err != nil {
		return err
	}

	_, err = io.Copy(destFile, srcFile)
	if err != nil {
		return err
	}

	return nil
}

// CreateSymlink creates a symlink with name linkName to file destName in
// workingDir
func CreateSymlink(t *testing.T, destName, linkName string) error {
	t.Helper()
	return os.Symlink(destName, linkName)
}

// ExampleAppsRevision contains the signed and unsigned commits used by the Git tests.
const ExampleAppsRevision = "8088f4c0d970abb09e250248cc97e35623447cb5"

// ServeFixture serves pinned files over HTTPS when the Nix fixture directory is configured.
// Other callers can use the upstream endpoint with their system trust store.
func ServeFixture(t *testing.T, directory, upstream string) (baseURL, caFile string) {
	t.Helper()
	root := os.Getenv("ARGOCD_TEST_FIXTURES")
	if root == "" {
		return upstream, ""
	}

	srv := httptest.NewTLSServer(http.FileServer(http.Dir(filepath.Join(root, directory))))
	t.Cleanup(srv.Close)
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(caFile, cert, 0o600))
	return srv.URL, caFile
}
