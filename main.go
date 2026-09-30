package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "jsonlink: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("jsonlink", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: jsonlink [options] [file.json ...]")
		fmt.Fprintln(stderr, "With no file arguments, one JSON document is read from stdin.")
		fmt.Fprintln(stderr, "Each file may contain one object, an object array, or {\"objects\": [...]}.")
		flags.PrintDefaults()
	}

	imagePath := flags.String("o", "", "write the linked binary image to this path (instead of only reporting it)")
	reportPath := flags.String("report", "", "write the JSON report to this path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	allObjects := make([]Object, 0)
	for _, inputPath := range flags.Args() {
		data, err := os.ReadFile(inputPath)
		if err != nil {
			return err
		}
		objects, err := ParseInput(data, filepath.Base(inputPath))
		if err != nil {
			return fmt.Errorf("%s: %w", inputPath, err)
		}
		allObjects = append(allObjects, objects...)
	}

	if flags.NArg() == 0 {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		objects, err := ParseInput(data, "stdin")
		if err != nil {
			return err
		}
		allObjects = objects
	}

	// Linking must finish and the report must marshal successfully before any
	// output file is created or replaced.
	report, err := Link(allObjects)
	if err != nil {
		return err
	}
	image, err := base64.StdEncoding.DecodeString(report.ImageBase64)
	if err != nil {
		return err
	}

	var encodedReport bytes.Buffer
	encoder := json.NewEncoder(&encodedReport)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		return err
	}

	if *imagePath != "" && *reportPath != "" {
		same, err := sameOutputPath(*imagePath, *reportPath)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("image path %q and report path %q refer to the same file; choose different outputs", *imagePath, *reportPath)
		}
	}

	// Stage every output in a temporary file first, then rename them all into
	// place. A failure while staging any output leaves every existing file
	// untouched, so the image and the report never diverge on disk.
	type stagedOutput struct {
		path    string
		tmpPath string
	}
	var staged []stagedOutput
	committed := false
	defer func() {
		if !committed {
			for _, output := range staged {
				_ = os.Remove(output.tmpPath)
			}
		}
	}()

	if *imagePath != "" {
		tmpPath, err := stageFile(*imagePath, image, 0o666)
		if err != nil {
			return fmt.Errorf("write image: %w", err)
		}
		staged = append(staged, stagedOutput{path: *imagePath, tmpPath: tmpPath})
	}
	if *reportPath != "" {
		tmpPath, err := stageFile(*reportPath, encodedReport.Bytes(), 0o666)
		if err != nil {
			return fmt.Errorf("write report: %w", err)
		}
		staged = append(staged, stagedOutput{path: *reportPath, tmpPath: tmpPath})
	}
	for _, output := range staged {
		if err := os.Rename(output.tmpPath, output.path); err != nil {
			return fmt.Errorf("replace %s: %w", output.path, err)
		}
	}
	committed = true

	if *imagePath == "" && *reportPath == "" {
		if _, err := stdout.Write(encodedReport.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// sameOutputPath reports whether two paths name the same file.
func sameOutputPath(first, second string) (bool, error) {
	firstAbs, err := filepath.Abs(first)
	if err != nil {
		return false, err
	}
	secondAbs, err := filepath.Abs(second)
	if err != nil {
		return false, err
	}
	if firstAbs == secondAbs {
		return true, nil
	}
	// If both files already exist, also catch aliases such as hard links.
	firstInfo, firstErr := os.Stat(firstAbs)
	secondInfo, secondErr := os.Stat(secondAbs)
	if firstErr == nil && secondErr == nil {
		return os.SameFile(firstInfo, secondInfo), nil
	}
	return false, nil
}

// stageFile writes data to a temporary file next to path and returns the
// temporary path. The caller is responsible for renaming or removing it.
func stageFile(path string, data []byte, perm os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}

	file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := file.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return tmpPath, nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmpPath, err := stageFile(path, data, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
