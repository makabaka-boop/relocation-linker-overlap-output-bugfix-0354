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

	if *imagePath != "" && *reportPath != "" {
		same, err := samePath(*imagePath, *reportPath)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("image path and report path must differ, both resolve to %q", *imagePath)
		}
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

	// 映像和报告必须一起提交：先暂存全部临时文件，再逐个原子替换；
	// 任一替换失败都会把已替换的文件回滚为原内容，避免磁盘上只剩一个已更新产物。
	outputs := make([]outputFile, 0, 2)
	if *imagePath != "" {
		outputs = append(outputs, outputFile{path: *imagePath, data: image, label: "image"})
	}
	if *reportPath != "" {
		outputs = append(outputs, outputFile{path: *reportPath, data: encodedReport.Bytes(), label: "report"})
	}
	if len(outputs) > 0 {
		if err := atomicWriteFiles(outputs, 0o666); err != nil {
			return err
		}
	} else {
		if _, err := stdout.Write(encodedReport.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

type outputFile struct {
	path  string
	data  []byte
	label string
}

func samePath(a, b string) (bool, error) {
	absoluteA, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	absoluteB, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return filepath.Clean(absoluteA) == filepath.Clean(absoluteB), nil
}

// atomicWriteFiles stages every output in a temporary file first, then replaces
// the destination paths one by one. A failure during replacement restores the
// previously replaced destinations so callers never observe a partially
// updated image/report pair.
func atomicWriteFiles(outputs []outputFile, perm os.FileMode) error {
	type staged struct {
		output  outputFile
		tmpPath string
	}

	stagedFiles := make([]staged, 0, len(outputs))
	cleanupStaged := true
	defer func() {
		if cleanupStaged {
			for _, staged := range stagedFiles {
				_ = os.Remove(staged.tmpPath)
			}
		}
	}()

	for _, output := range outputs {
		dir := filepath.Dir(output.path)
		if dir == "" {
			dir = "."
		}
		if info, err := os.Stat(output.path); err == nil && info.IsDir() {
			return fmt.Errorf("write %s: %s is a directory", output.label, output.path)
		}

		file, err := os.CreateTemp(dir, filepath.Base(output.path)+".*.tmp")
		if err != nil {
			return fmt.Errorf("write %s: %w", output.label, err)
		}
		tmpPath := file.Name()
		if _, err := file.Write(output.data); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("write %s: %w", output.label, err)
		}
		if err := file.Chmod(perm); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("write %s: %w", output.label, err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("write %s: %w", output.label, err)
		}
		stagedFiles = append(stagedFiles, staged{output: output, tmpPath: tmpPath})
	}

	type committed struct {
		output     outputFile
		backupPath string
		hadBackup  bool
	}
	committedFiles := make([]committed, 0, len(stagedFiles))

	rollback := func(commitErr error) error {
		for i := len(committedFiles) - 1; i >= 0; i-- {
			done := committedFiles[i]
			if done.hadBackup {
				_ = os.Rename(done.backupPath, done.output.path)
			} else {
				_ = os.Remove(done.output.path)
			}
		}
		return commitErr
	}

	for _, staged := range stagedFiles {
		var backupPath string
		hadBackup := false
		if _, err := os.Stat(staged.output.path); err == nil {
			backup, err := os.CreateTemp(filepath.Dir(staged.output.path),
				filepath.Base(staged.output.path)+".*.bak")
			if err != nil {
				return rollback(fmt.Errorf("write %s: %w", staged.output.label, err))
			}
			backupPath = backup.Name()
			if err := backup.Close(); err != nil {
				_ = os.Remove(backupPath)
				return rollback(fmt.Errorf("write %s: %w", staged.output.label, err))
			}
			if err := os.Rename(staged.output.path, backupPath); err != nil {
				_ = os.Remove(backupPath)
				return rollback(fmt.Errorf("write %s: %w", staged.output.label, err))
			}
			hadBackup = true
		}

		if err := os.Rename(staged.tmpPath, staged.output.path); err != nil {
			if hadBackup {
				_ = os.Rename(backupPath, staged.output.path)
			}
			return rollback(fmt.Errorf("write %s: %w", staged.output.label, err))
		}
		committedFiles = append(committedFiles, committed{
			output:     staged.output,
			backupPath: backupPath,
			hadBackup:  hadBackup,
		})
	}

	for _, done := range committedFiles {
		if done.hadBackup {
			_ = os.Remove(done.backupPath)
		}
	}
	cleanupStaged = false
	return nil
}
