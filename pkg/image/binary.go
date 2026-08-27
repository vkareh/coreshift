package image

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractBinary copies binaryPath from an unpacked image filesystem into
// targetDirectory. Optional extra library paths are copied into the same
// directory, providing an extension point for binaries with non-host
// dependencies.
func ExtractBinary(filesystemRoot, binaryPath, targetDirectory string, extraLibraries ...[]string) error {
	if len(extraLibraries) > 1 {
		return fmt.Errorf("at most one extra library list is supported")
	}
	if err := os.MkdirAll(targetDirectory, 0o755); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}

	paths := []string{binaryPath}
	if len(extraLibraries) == 1 {
		paths = append(paths, extraLibraries[0]...)
	}
	for _, sourcePath := range paths {
		source, err := imagePath(filesystemRoot, sourcePath)
		if err != nil {
			return err
		}
		if err := copyFile(source, filepath.Join(targetDirectory, filepath.Base(sourcePath))); err != nil {
			return fmt.Errorf("extract %q: %w", sourcePath, err)
		}
	}
	return nil
}

func imagePath(root, imagePath string) (string, error) {
	imagePath = filepath.Clean(imagePath)
	imagePath = strings.TrimPrefix(imagePath, string(filepath.Separator))
	if imagePath == "." || imagePath == ".." || strings.HasPrefix(imagePath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image path escapes filesystem: %q", imagePath)
	}
	joined := filepath.Join(root, imagePath)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image path escapes filesystem: %q", imagePath)
	}
	return joined, nil
}

func copyFile(sourcePath, targetPath string) error {
	input, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer input.Close()

	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}

	output, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(targetPath, info.Mode().Perm()); err != nil {
		return err
	}
	return nil
}
