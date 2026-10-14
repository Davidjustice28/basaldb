package engine

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func validHeader() *Header {
	return &Header{
		Version: CurrentVersion,
		Format:  FormatMatrixSlottedPage,
		Layers: map[int]Layer{
			0: {Height: 0, Name: "Accounts"},
			1: {Height: 1, Name: "Teams"},
			2: {Height: 2, Name: "Users"},
		},
		Branches:       []string{"main"},
		CommitLogStart: HeaderBlockSize,
		CommitLogEnd:   HeaderBlockSize + 512,
		DataStart:      HeaderBlockSize + 512,
		DataEnd:        HeaderBlockSize + 4096,
		JournalStart:   HeaderBlockSize + 4096,
	}
}

func TestParseHeaderValid(t *testing.T) {
	h := validHeader()
	block, err := h.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(block) != HeaderBlockSize {
		t.Fatalf("block size = %d, want %d", len(block), HeaderBlockSize)
	}

	got, err := ParseHeader(block)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if got.Version != CurrentVersion || got.Format != FormatMatrixSlottedPage {
		t.Errorf("version/format = %q/%q", got.Version, got.Format)
	}
	if len(got.Layers) != 3 {
		t.Fatalf("layers = %d, want 3", len(got.Layers))
	}
	if l := got.Layers[1]; l.Name != "Teams" || l.Height != 1 {
		t.Errorf("layer 1 = %+v, want Teams", l)
	}
	if got.CommitLogStart != h.CommitLogStart || got.CommitLogEnd != h.CommitLogEnd {
		t.Errorf("commit log boundaries = %d-%d, want %d-%d",
			got.CommitLogStart, got.CommitLogEnd, h.CommitLogStart, h.CommitLogEnd)
	}
	if got.DataStart != h.DataStart || got.DataEnd != h.DataEnd || got.JournalStart != h.JournalStart {
		t.Errorf("data/journal boundaries wrong: %+v", got)
	}
	if len(got.Branches) != 1 || got.Branches[0] != "main" {
		t.Errorf("branches = %v", got.Branches)
	}
}

func TestParseHeaderRawLayout(t *testing.T) {
	// The exact textual layout shown in the project spec, including the
	// sample layer names and zero-padded offsets style.
	raw := "Magic:BSL\n" +
		"Version:1.0\n" +
		"Layers:[0:Accounts, 1:Teams, 2:Users]\n" +
		"Format:MatrixSlottedPage\n" +
		"CommitLogStart:4096\n" +
		"CommitLogEnd:8192\n" +
		"DataStart:8192\n" +
		"DataEnd:16384\n" +
		"JournalStart:16384\n"
	got, err := ParseHeader([]byte(raw))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if _, ok := got.LayerByName("Users"); !ok {
		t.Error("Users layer not found")
	}
	if l, ok := got.LayerByName("Accounts"); !ok || l.Height != 0 {
		t.Errorf("Accounts layer = %+v ok=%v", l, ok)
	}
}

func TestParseHeaderCorrupted(t *testing.T) {
	good := "Magic:BSL\nVersion:1.0\nFormat:MatrixSlottedPage\n" +
		"Layers:[0:Accounts, 1:Teams, 2:Users]\n" +
		"CommitLogStart:4096\nCommitLogEnd:8192\nDataStart:8192\nDataEnd:16384\nJournalStart:16384\n"

	cases := []struct {
		name    string
		block   string
		wantErr error
	}{
		{"empty", "", ErrCorruptHeader},
		{"wrong magic", strings.Replace(good, "Magic:BSL", "Magic:BSX", 1), ErrBadMagic},
		{"missing magic", strings.Replace(good, "Magic:BSL\n", "", 1), ErrBadMagic},
		{"unsupported version", strings.Replace(good, "Version:1.0", "Version:9.9", 1), ErrUnsupportedVersion},
		{"missing version", strings.Replace(good, "Version:1.0\n", "", 1), ErrCorruptHeader},
		{"missing layers", strings.Replace(good, "Layers:[0:Accounts, 1:Teams, 2:Users]\n", "", 1), ErrCorruptHeader},
		{"malformed layers", strings.Replace(good, "Layers:[0:Accounts, 1:Teams, 2:Users]", "Layers:0:Accounts", 1), ErrCorruptHeader},
		{"bad layer height", strings.Replace(good, "0:Accounts", "x:Accounts", 1), ErrCorruptHeader},
		{"duplicate layer height", strings.Replace(good, "1:Teams", "0:Teams", 1), ErrCorruptHeader},
		{"empty layer name", strings.Replace(good, "2:Users", "2:", 1), ErrCorruptHeader},
		{"bad offset", strings.Replace(good, "DataStart:8192", "DataStart:banana", 1), ErrCorruptHeader},
		{"inverted offsets", strings.Replace(good, "CommitLogEnd:8192", "CommitLogEnd:4095", 1), ErrCorruptHeader},
		{"offset below header", strings.Replace(good, "CommitLogStart:4096", "CommitLogStart:16", 1), ErrCorruptHeader},
		{"malformed line", strings.Replace(good, "Format:MatrixSlottedPage", "FormatMatrixSlottedPage", 1), ErrCorruptHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHeader([]byte(tc.block))
			if err == nil {
				t.Fatalf("ParseHeader succeeded, want error %v", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestHeaderMarshalRejectsInvalid(t *testing.T) {
	h := validHeader()
	h.Version = "0.0"
	if _, err := h.Marshal(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Marshal err = %v, want %v", err, ErrUnsupportedVersion)
	}
}

func TestHeaderNulPaddingTolerated(t *testing.T) {
	h := validHeader()
	block, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	nulled := make([]byte, HeaderBlockSize)
	copy(nulled, bytes.TrimRight(block, "\n"))
	got, err := ParseHeader(nulled)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if got.DataEnd != h.DataEnd {
		t.Errorf("DataEnd = %d, want %d", got.DataEnd, h.DataEnd)
	}
}

func TestHeaderRoundTripsAttributeNames(t *testing.T) {
	h := validHeader()
	h.Layers[0] = Layer{Height: 0, Name: "Accounts", Attrs: []string{"name", "joined"}}
	block, err := h.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(block[:HeaderBlockSize]), "0:Accounts(name,joined)") {
		t.Errorf("header lacks attribute declaration:\n%s", string(block[:200]))
	}
	got, err := ParseHeader(block)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	l := got.Layers[0]
	if len(l.Attrs) != 2 || l.Attrs[0] != "name" || l.Attrs[1] != "joined" {
		t.Errorf("layer 0 attrs = %v", l.Attrs)
	}
	if i, ok := l.AttrIndex("joined"); !ok || i != 1 {
		t.Errorf("AttrIndex(joined) = %d, %v", i, ok)
	}
	if _, ok := l.AttrIndex("missing"); ok {
		t.Error("AttrIndex(missing) succeeded")
	}
}

func TestParseLayersSpec(t *testing.T) {
	cases := []struct {
		spec string
		want []Layer
	}{
		{"Accounts,Teams", []Layer{
			{Height: 0, Name: "Accounts"},
			{Height: 1, Name: "Teams"},
		}},
		{"0:Accounts(name,date),2:Users(name,email)", []Layer{
			{Height: 0, Name: "Accounts", Attrs: []string{"name", "date"}},
			{Height: 2, Name: "Users", Attrs: []string{"name", "email"}},
		}},
	}
	for _, tc := range cases {
		got, err := ParseLayers(tc.spec)
		if err != nil {
			t.Errorf("ParseLayers(%q): %v", tc.spec, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("ParseLayers(%q) = %+v", tc.spec, got)
			continue
		}
		for i := range got {
			if got[i].Height != tc.want[i].Height ||
				got[i].Name != tc.want[i].Name ||
				strings.Join(got[i].Attrs, ",") != strings.Join(tc.want[i].Attrs, ",") {
				t.Errorf("ParseLayers(%q)[%d] = %+v, want %+v", tc.spec, i, got[i], tc.want[i])
			}
		}
	}
	for _, bad := range []string{"", "0:Accounts(name,),1:Teams", "x:Accounts", "Accounts(nope", "0:A,0:B"} {
		if _, err := ParseLayers(bad); err == nil {
			t.Errorf("ParseLayers(%q) succeeded, want error", bad)
		}
	}
}

func TestValidateRejectsDuplicateAttributes(t *testing.T) {
	h := validHeader()
	h.Layers[0] = Layer{Height: 0, Name: "Accounts", Attrs: []string{"name", "name"}}
	if _, err := h.Marshal(); !errors.Is(err, ErrCorruptHeader) {
		t.Errorf("Marshal with duplicate attrs = %v, want ErrCorruptHeader", err)
	}
	h.Layers[0] = Layer{Height: 0, Name: "Accounts", Attrs: []string{"name", ""}}
	if _, err := h.Marshal(); !errors.Is(err, ErrCorruptHeader) {
		t.Errorf("Marshal with empty attr = %v, want ErrCorruptHeader", err)
	}
}
