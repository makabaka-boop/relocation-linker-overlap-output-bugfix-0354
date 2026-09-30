package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hexBytes(value string) ByteSlice {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return ByteSlice(decoded)
}

func int64Ptr(value int64) *int64 {
	return &value
}

func handCalculatedObjects() []Object {
	return []Object{
		{
			Name: "a.o",
			Text: &Section{Bytes: hexBytes("0304"), Align: 1},
			Data: &Section{Bytes: hexBytes("0a000000000b"), Align: 2},
			Symbols: []Symbol{
				{Name: "private", Section: ".text", Offset: 0, Scope: "local"},
				{Name: "shared", Section: ".data", Offset: 0, Scope: "global", Binding: "weak"},
			},
			Relocations: []Relocation{
				{Type: "PCREL16", Section: ".text", Offset: 0, Symbol: "private"},
				{Type: "ABS32", Section: ".data", Offset: 1, Symbol: "shared", Addend: int64Ptr(4)},
			},
		},
		{
			Name: "b.o",
			Text: &Section{Bytes: hexBytes("05060708"), Align: 4},
			Data: &Section{Bytes: hexBytes("0d000000000f"), Align: 4},
			Symbols: []Symbol{
				{Name: "private", Section: ".text", Offset: 0, Scope: "local"},
				{Name: "shared", Section: ".data", Offset: 0, Scope: "global", Binding: "strong"},
			},
			Relocations: []Relocation{
				{Type: "PCREL16", Section: ".text", Offset: 2, Symbol: "private"},
				{Type: "ABS32", Section: ".data", Offset: 1, Symbol: "shared"},
			},
		},
	}
}

func TestLinkHandCalculatedLayoutSymbolsAndRelocations(t *testing.T) {
	report, err := Link(handCalculatedObjects())
	if err != nil {
		t.Fatal(err)
	}

	// .text a.o: 0x1000..0x1002
	// .text b.o: align to 4 -> 0x1004..0x1008
	// .data a.o: 0x1008..0x100e
	// .data b.o: align to 4 -> 0x1010..0x1016
	const expectedHex = "feff00000506fcff0a141000000b00000d101000000f"
	image, err := base64.StdEncoding.DecodeString(report.ImageBase64)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(image); got != expectedHex {
		t.Fatalf("image = %s, want %s", got, expectedHex)
	}
	if report.ImageSize != 0x16 {
		t.Fatalf("image size = %d, want 0x16", report.ImageSize)
	}

	wantSections := []struct {
		object  string
		section string
		address int
		bytes   string
	}{
		{"a.o", ".text", 0x1000, "feff"},
		{"b.o", ".text", 0x1004, "0506fcff"},
		{"a.o", ".data", 0x1008, "0a141000000b"},
		{"b.o", ".data", 0x1010, "0d101000000f"},
	}
	if len(report.Sections) != len(wantSections) {
		t.Fatalf("section count = %d, want %d", len(report.Sections), len(wantSections))
	}
	for i, want := range wantSections {
		got := report.Sections[i]
		if got.ObjectName != want.object || got.Section != want.section ||
			got.Address != want.address || got.BytesHex != want.bytes {
			t.Fatalf("section %d = %+v, want object=%s section=%s address=%#x bytes=%s",
				i, got, want.object, want.section, want.address, want.bytes)
		}
	}

	symbols := make(map[string]SymbolReport)
	for _, symbol := range report.Symbols {
		// Two object-local symbols intentionally have the same name.
		symbols[symbol.ObjectName+"/"+symbol.Name] = symbol
		if _, global := symbols[symbol.Name]; !global {
			symbols[symbol.Name] = symbol
		}
	}
	if symbols["a.o/private"].Address != 0x1000 || !symbols["a.o/private"].Selected {
		t.Fatalf("a.o private symbol = %+v", symbols["a.o/private"])
	}
	if symbols["b.o/private"].Address != 0x1004 || !symbols["b.o/private"].Selected {
		t.Fatalf("b.o private symbol = %+v", symbols["b.o/private"])
	}
	if symbols["a.o/shared"].Address != 0x1008 || symbols["a.o/shared"].Selected {
		t.Fatalf("weak shared symbol = %+v, want unselected at 0x1008", symbols["a.o/shared"])
	}
	if symbols["b.o/shared"].Address != 0x1010 || !symbols["b.o/shared"].Selected {
		t.Fatalf("strong shared symbol = %+v, want selected at 0x1010", symbols["b.o/shared"])
	}

	wantRelocations := []struct {
		object       string
		typ          string
		start        int
		end          int
		target       int
		targetObject string
		value        int64
		before       string
		after        string
	}{
		{"a.o", "PCREL16", 0x1000, 0x1002, 0x1000, "a.o", -2, "0304", "feff"},
		{"a.o", "ABS32", 0x1009, 0x100d, 0x1010, "b.o", 0x1014, "00000000", "14100000"},
		{"b.o", "PCREL16", 0x1006, 0x1008, 0x1004, "b.o", -4, "0708", "fcff"},
		{"b.o", "ABS32", 0x1011, 0x1015, 0x1010, "b.o", 0x1010, "00000000", "10100000"},
	}
	if len(report.Relocations) != len(wantRelocations) {
		t.Fatalf("relocation count = %d, want %d", len(report.Relocations), len(wantRelocations))
	}
	for i, want := range wantRelocations {
		got := report.Relocations[i]
		if got.ObjectName != want.object || got.Type != want.typ ||
			got.PatchStart != want.start || got.PatchEnd != want.end ||
			got.TargetAddress != want.target || got.DefinedInObjectName != want.targetObject ||
			got.ComputedValue != want.value || got.BytesBefore != want.before ||
			got.BytesAfter != want.after {
			t.Fatalf("relocation %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestLinkAlignmentEightAndSixteen(t *testing.T) {
	objects := []Object{
		{
			Name: "a.o",
			Text: &Section{Bytes: ByteSlice{0xaa}, Align: 8},
			Data: &Section{Bytes: ByteSlice{0xcc}, Align: 8},
		},
		{
			Name: "b.o",
			Text: &Section{Bytes: ByteSlice{0xbb}, Align: 16},
			Data: &Section{Bytes: ByteSlice{0xdd}, Align: 16},
		},
	}

	report, err := Link(objects)
	if err != nil {
		t.Fatal(err)
	}
	addresses := []int{0x1000, 0x1010, 0x1018, 0x1020}
	for i, want := range addresses {
		if got := report.Sections[i].Address; got != want {
			t.Fatalf("%s %s address = %#x, want %#x",
				report.Sections[i].ObjectName, report.Sections[i].Section, got, want)
		}
	}
}

func TestPCREL16SignedBoundaries(t *testing.T) {
	objects := []Object{{
		Name: "bounds.o",
		Text: &Section{Bytes: make(ByteSlice, 0x8002), Align: 1},
		Symbols: []Symbol{
			{Name: "front", Section: ".text", Offset: 0, Scope: "local"},
			{Name: "back", Section: ".text", Offset: 0x8001, Scope: "local"},
		},
		Relocations: []Relocation{
			{Type: "PCREL16", Section: ".text", Offset: 0, Symbol: "back"},
			{Type: "PCREL16", Section: ".text", Offset: 0x7ffe, Symbol: "front"},
		},
	}}

	report, err := Link(objects)
	if err != nil {
		t.Fatal(err)
	}
	if report.Relocations[0].ComputedValue != math.MaxInt16 ||
		report.Relocations[0].BytesAfter != "ff7f" {
		t.Fatalf("maximum positive PCREL16 = %+v", report.Relocations[0])
	}
	if report.Relocations[1].ComputedValue != math.MinInt16 ||
		report.Relocations[1].BytesAfter != "0080" {
		t.Fatalf("minimum negative PCREL16 = %+v", report.Relocations[1])
	}
}

func TestLinkRejectsInvalidInputWithoutPartialImage(t *testing.T) {
	tests := []struct {
		name    string
		objects []Object
		wantErr string
	}{
		{
			name: "duplicate strong globals",
			objects: []Object{
				{
					Name: "a.o",
					Text: &Section{Bytes: ByteSlice{1}},
					Symbols: []Symbol{
						{Name: "x", Section: ".text", Scope: "global", Binding: "strong"},
					},
				},
				{
					Name: "b.o",
					Text: &Section{Bytes: ByteSlice{2}},
					Symbols: []Symbol{
						{Name: "x", Section: ".text", Scope: "global", Binding: "strong"},
					},
				},
			},
			wantErr: "duplicate strong global symbol",
		},
		{
			name: "unresolved symbol",
			objects: []Object{{
				Name: "a.o",
				Text: &Section{Bytes: ByteSlice{0, 0}},
				Relocations: []Relocation{
					{Type: "PCREL16", Section: ".text", Offset: 0, Symbol: "missing"},
				},
			}},
			wantErr: "unresolved symbol",
		},
		{
			name: "local symbol is not visible in another object",
			objects: []Object{
				{
					Name:    "a.o",
					Text:    &Section{Bytes: ByteSlice{0}},
					Symbols: []Symbol{{Name: "local_x", Section: ".text", Scope: "local"}},
				},
				{
					Name: "b.o",
					Text: &Section{Bytes: ByteSlice{0, 0}},
					Relocations: []Relocation{
						{Type: "PCREL16", Section: ".text", Offset: 0, Symbol: "local_x"},
					},
				},
			},
			wantErr: "unresolved symbol",
		},
		{
			name: "PCREL16 positive overflow",
			objects: []Object{{
				Name:    "a.o",
				Text:    &Section{Bytes: ByteSlice{0, 0}},
				Symbols: []Symbol{{Name: "here", Section: ".text", Scope: "local"}},
				Relocations: []Relocation{{
					Type:    "PCREL16",
					Section: ".text",
					Offset:  0,
					Symbol:  "here",
					Addend:  int64Ptr(40000),
				}},
			}},
			wantErr: "PCREL16 displacement",
		},
		{
			name: "ABS32 overflow",
			objects: []Object{{
				Name:    "a.o",
				Text:    &Section{Bytes: ByteSlice{0, 0, 0, 0}},
				Symbols: []Symbol{{Name: "here", Section: ".text", Scope: "local"}},
				Relocations: []Relocation{{
					Type:    "ABS32",
					Section: ".text",
					Offset:  0,
					Symbol:  "here",
					Addend:  int64Ptr(0xffffffff),
				}},
			}},
			wantErr: "ABS32 value",
		},
		{
			name: "patch extends past section",
			objects: []Object{{
				Name:    "a.o",
				Text:    &Section{Bytes: ByteSlice{0, 0, 0}},
				Symbols: []Symbol{{Name: "here", Section: ".text", Scope: "local"}},
				Relocations: []Relocation{
					{Type: "ABS32", Section: ".text", Offset: 1, Symbol: "here"},
				},
			}},
			wantErr: "does not fit",
		},
		{
			name: "invalid alignment",
			objects: []Object{{
				Name: "a.o",
				Text: &Section{Bytes: ByteSlice{0}, Align: 3},
			}},
			wantErr: "alignment",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := Link(test.objects)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Link error = %v, want substring %q", err, test.wantErr)
			}
			if report != nil {
				t.Fatalf("report = %+v, want nil on error", report)
			}
		})
	}
}

func TestWeakGlobalKeepsFirstDefinitionWhenNoStrongExists(t *testing.T) {
	objects := []Object{
		{
			Name: "first.o",
			Text: &Section{Bytes: ByteSlice{0xaa}, Align: 1},
			Symbols: []Symbol{
				{Name: "w", Section: ".text", Scope: "global", Binding: "weak"},
			},
		},
		{
			Name: "second.o",
			Text: &Section{Bytes: ByteSlice{0xbb}, Align: 1},
			Symbols: []Symbol{
				{Name: "w", Section: ".text", Scope: "global", Binding: "weak"},
			},
		},
	}

	report, err := Link(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range report.Symbols {
		wantSelected := symbol.ObjectName == "first.o"
		if symbol.Selected != wantSelected {
			t.Fatalf("%s selected = %v, want %v", symbol.ObjectName, symbol.Selected, wantSelected)
		}
	}
}

func TestRunDoesNotReplaceOutputWhenLinkFails(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.bin")
	original := []byte("keep this image")
	if err := atomicWriteFiles([]outputFile{{path: imagePath, data: original, label: "image"}}, 0o666); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := run([]string{"-o", imagePath}, strings.NewReader(`{
		"name": "bad.o",
		"text": {"bytes": [0, 0], "align": 1},
		"relocations": [{"type": "PCREL16", "section": ".text", "offset": 0, "symbol": "missing"}]
	}`), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected link failure")
	}
	got, readErr := readFileForTest(imagePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("output file changed: got %q, want %q", got, original)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func readFileForTest(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func TestLinkRejectsOverlappingRelocationPatches(t *testing.T) {
	objects := []Object{{
		Name: "overlap.o",
		// 8 字节 .data：两条 ABS32 分别落在偏移 0 和 2，区域相交。
		Data: &Section{Bytes: make(ByteSlice, 8), Align: 1},
		Symbols: []Symbol{
			{Name: "here", Section: ".data", Offset: 0, Scope: "local"},
		},
		Relocations: []Relocation{
			{Type: "ABS32", Section: ".data", Offset: 0, Symbol: "here"},
			{Type: "ABS32", Section: ".data", Offset: 2, Symbol: "here"},
		},
	}}

	report, err := Link(objects)
	if err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("Link error = %v, want overlap error", err)
	}
	if report != nil {
		t.Fatalf("report = %+v, want nil on overlap", report)
	}
}

func TestLinkAcceptsAdjacentRelocationPatches(t *testing.T) {
	objects := []Object{{
		Name: "adjacent.o",
		// 两条 PCREL16 首尾相接：0..2 和 2..4，不相交。
		Text: &Section{Bytes: make(ByteSlice, 4), Align: 1},
		Symbols: []Symbol{
			{Name: "here", Section: ".text", Offset: 4, Scope: "local"},
		},
		Relocations: []Relocation{
			{Type: "PCREL16", Section: ".text", Offset: 0, Symbol: "here"},
			{Type: "PCREL16", Section: ".text", Offset: 2, Symbol: "here"},
		},
	}}

	if _, err := Link(objects); err != nil {
		t.Fatalf("adjacent patches must be allowed, got %v", err)
	}
}

const validInputJSON = `{
	"name": "ok.o",
	"data": {"bytes": [0, 0, 0, 0], "align": 1},
	"symbols": [{"name": "g", "section": ".data", "offset": 0, "scope": "global", "binding": "strong"}],
	"relocations": [{"type": "ABS32", "section": ".data", "offset": 0, "symbol": "g"}]
}`

func TestRunRejectsIdenticalImageAndReportPaths(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "out.bin")
	original := []byte("keep this image")
	if err := atomicWriteFiles([]outputFile{{path: imagePath, data: original, label: "image"}}, 0o666); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// 两个路径文本不同（带 "./" 段），但清理后指向同一个文件。
	reportArg := filepath.Join(dir, ".", "out.bin")
	err := run([]string{"-o", imagePath, "-report", reportArg},
		strings.NewReader(validInputJSON), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("run error = %v, want path mismatch error", err)
	}

	got, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("existing file changed after rejected paths: got %q", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunRollsImageBackWhenReportCommitFails(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.bin")
	staleImage := []byte("stale image contents")
	if err := os.WriteFile(imagePath, staleImage, 0o666); err != nil {
		t.Fatal(err)
	}

	// 已存在的目录无法作为文件替换目标，映像提交后报告提交必然失败。
	reportDir := filepath.Join(dir, "report.json")
	if err := os.Mkdir(reportDir, 0o777); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := run([]string{"-o", imagePath, "-report", reportDir},
		strings.NewReader(validInputJSON), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "write report") {
		t.Fatalf("run error = %v, want write report error", err)
	}

	got, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("image missing after rollback: %v", err)
	}
	if !bytes.Equal(got, staleImage) {
		t.Fatalf("image after failed commit = %q, want stale %q", got, staleImage)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") || strings.Contains(entry.Name(), ".bak") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestRunImageAndReportStayConsistent(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.bin")
	reportPath := filepath.Join(dir, "report.json")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"-o", imagePath, "-report", reportPath},
		strings.NewReader(validInputJSON), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	image, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}

	var report Report
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		t.Fatal(err)
	}
	encoded, err := base64.StdEncoding.DecodeString(report.ImageBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, image) {
		t.Fatal("report image_base64 does not match the image written to disk")
	}

	// 报告里每条重定位的补丁后字节都必须与最终映像中的实际字节一致。
	for _, relocation := range report.Relocations {
		start := relocation.PatchStart - report.ImageBase
		want, err := hex.DecodeString(relocation.BytesAfter)
		if err != nil {
			t.Fatal(err)
		}
		if got := image[start : start+len(want)]; !bytes.Equal(got, want) {
			t.Fatalf("relocation %s bytes_after %s does not match final image %s",
				relocation.PatchStartHex, relocation.BytesAfter, hex.EncodeToString(got))
		}
	}
}
