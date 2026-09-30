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
			name: "overlapping patches",
			objects: []Object{{
				Name:    "a.o",
				Text:    &Section{Bytes: ByteSlice{0, 0, 0, 0}},
				Symbols: []Symbol{{Name: "here", Section: ".text", Scope: "local"}},
				Relocations: []Relocation{
					{Type: "ABS32", Section: ".text", Offset: 0, Symbol: "here"},
					{Type: "PCREL16", Section: ".text", Offset: 2, Symbol: "here"},
				},
			}},
			wantErr: "overlaps",
		},
		{
			name: "identical patch ranges",
			objects: []Object{{
				Name:    "a.o",
				Text:    &Section{Bytes: ByteSlice{0, 0, 0, 0}},
				Symbols: []Symbol{{Name: "here", Section: ".text", Scope: "local"}},
				Relocations: []Relocation{
					{Type: "ABS32", Section: ".text", Offset: 0, Symbol: "here"},
					{Type: "ABS32", Section: ".text", Offset: 0, Symbol: "here"},
				},
			}},
			wantErr: "overlaps",
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

func TestLinkAdjacentPatchesMatchFinalImage(t *testing.T) {
	objects := []Object{{
		Name:    "a.o",
		Text:    &Section{Bytes: make(ByteSlice, 6)},
		Symbols: []Symbol{{Name: "here", Section: ".text", Offset: 0, Scope: "local"}},
		Relocations: []Relocation{
			{Type: "ABS32", Section: ".text", Offset: 0, Symbol: "here"},
			{Type: "PCREL16", Section: ".text", Offset: 4, Symbol: "here"},
		},
	}}

	report, err := Link(objects)
	if err != nil {
		t.Fatal(err)
	}
	// ABS32 at 0x1000..0x1004 holds 0x1000; PCREL16 at 0x1004..0x1006 holds -6.
	const expectedHex = "00100000faff"
	image, err := base64.StdEncoding.DecodeString(report.ImageBase64)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(image); got != expectedHex {
		t.Fatalf("image = %s, want %s", got, expectedHex)
	}
	if report.Sections[0].BytesHex != expectedHex {
		t.Fatalf("section bytes = %s, want final image %s", report.Sections[0].BytesHex, expectedHex)
	}
	if report.Relocations[0].BytesAfter != "00100000" || report.Relocations[1].BytesAfter != "faff" {
		t.Fatalf("relocation bytes_after = %s, %s; want 00100000, faff",
			report.Relocations[0].BytesAfter, report.Relocations[1].BytesAfter)
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
	if err := atomicWriteFile(imagePath, original, 0o666); err != nil {
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

const validObjectJSON = `{
	"name": "ok.o",
	"text": {"bytes": [0, 0], "align": 1},
	"symbols": [{"name": "here", "section": ".text", "offset": 0, "scope": "local"}],
	"relocations": [{"type": "PCREL16", "section": ".text", "offset": 0, "symbol": "here"}]
}`

func TestRunWritesConsistentImageAndReport(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.bin")
	reportPath := filepath.Join(dir, "report.json")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"-o", imagePath, "-report", reportPath},
		strings.NewReader(validObjectJSON), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	image, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(image); got != "feff" {
		t.Fatalf("image = %s, want feff", got)
	}

	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(reportData, &report); err != nil {
		t.Fatal(err)
	}
	reportImage, err := base64.StdEncoding.DecodeString(report.ImageBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reportImage, image) {
		t.Fatalf("report image %x does not match image file %x", reportImage, image)
	}
	if report.Relocations[0].BytesAfter != hex.EncodeToString(image) {
		t.Fatalf("relocation bytes_after = %s, want final image bytes %s",
			report.Relocations[0].BytesAfter, hex.EncodeToString(image))
	}
}

func TestRunRejectsIdenticalImageAndReportPaths(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "out.bin")
	aliasedPath := filepath.Join(dir, ".", "out.bin")

	for _, reportPath := range []string{outputPath, aliasedPath} {
		var stdout, stderr bytes.Buffer
		err := run([]string{"-o", outputPath, "-report", reportPath},
			strings.NewReader(validObjectJSON), &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "same file") {
			t.Fatalf("run(-o %q -report %q) error = %v, want substring %q",
				outputPath, reportPath, err, "same file")
		}
		if _, statErr := os.Stat(outputPath); !os.IsNotExist(statErr) {
			t.Fatalf("output file exists after rejected run: %v", statErr)
		}
	}
}

func TestRunDoesNotReplaceImageWhenReportWriteFails(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "image.bin")
	original := []byte("keep this image")
	if err := atomicWriteFile(imagePath, original, 0o666); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dir, "missing", "report.json")

	var stdout, stderr bytes.Buffer
	err := run([]string{"-o", imagePath, "-report", reportPath},
		strings.NewReader(validObjectJSON), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected report write failure")
	}
	got, readErr := readFileForTest(imagePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("image file changed: got %q, want %q", got, original)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "image.bin" {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Fatalf("leftover files in output directory: %v", names)
	}
}
