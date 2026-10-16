// Package engine implements the BasalDB .bsl single-file container: an ASCII
// header block, a human-readable commit log region, slotted binary data
// pages, and an append-only delta journal.
//
// The store provides MVCC snapshot isolation (concurrent reads and writes
// never block each other) and git-style branching: named branches with
// independent commit histories (create / inspect / diverge, no merge).
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// MagicSignature must be the first non-empty line of every .bsl file.
	MagicSignature = "Magic:BSL"
	// VersionV10 is the original format: record tuples store child counts,
	// attribute counts and attribute lengths as uint16. A value past that
	// width used to truncate and make the file unopenable (issue #44).
	VersionV10 = "1.0"
	// CurrentVersion is the .bsl format version written by this engine.
	// 1.1 widens those tuple lengths to uint32. 1.0 files still open.
	CurrentVersion = "1.1"
	// FormatMatrixSlottedPage is the only data region format in v1.
	FormatMatrixSlottedPage = "MatrixSlottedPage"
	// HeaderBlockSize is the fixed size of the ASCII header region.
	HeaderBlockSize = 4096
)

// Errors returned when a .bsl header cannot be trusted.
var (
	ErrBadMagic           = errors.New("engine: bad magic signature")
	ErrCorruptHeader      = errors.New("engine: corrupt header")
	ErrUnsupportedVersion = errors.New("engine: unsupported version")
)

// Layer is one Y-axis height of the matrix: a named level of the hierarchy.
// Attrs optionally declares the layer's attribute names (Z axis, in
// positional order) so queries can reference attributes symbolically:
//
//	Layers:[0:Accounts(name,date), 1:Teams(name)]
//
// A trailing '!' on an attribute name (url!) declares a hash index used
// for equality lookups. Files written before attribute declarations
// existed simply have none.
type Layer struct {
	Height  int      `json:"height"`
	Name    string   `json:"name"`
	Attrs   []string `json:"attrs,omitempty"`
	Indexed []string `json:"indexed,omitempty"` // subset of Attrs, canonical spelling
}

// AttrIndex resolves a declared attribute name to its positional
// index. Matching is case-insensitive, so [Name], [name] and [NAME]
// all reference the same column.
func (l Layer) AttrIndex(name string) (int, bool) {
	for i, a := range l.Attrs {
		if strings.EqualFold(a, name) {
			return i, true
		}
	}
	return 0, false
}

// IsIndexed reports whether name is a declared hash-index attribute.
// Matching is case-insensitive.
func (l Layer) IsIndexed(name string) bool {
	for _, n := range l.Indexed {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// SchemaAttrs renders declared attributes for SHOW SCHEMA. Indexed names
// keep a trailing '!'.
func (l Layer) SchemaAttrs() string {
	on := map[string]bool{}
	for _, n := range l.Indexed {
		on[strings.ToLower(n)] = true
	}
	parts := make([]string, len(l.Attrs))
	for i, a := range l.Attrs {
		if on[strings.ToLower(a)] {
			parts[i] = a + "!"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, ", ")
}

// headerAttrs is the on-disk attribute list: comma-separated, no spaces,
// with '!' on indexed names.
func (l Layer) headerAttrs() string {
	on := map[string]bool{}
	for _, n := range l.Indexed {
		on[strings.ToLower(n)] = true
	}
	parts := make([]string, len(l.Attrs))
	for i, a := range l.Attrs {
		if on[strings.ToLower(a)] {
			parts[i] = a + "!"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, ",")
}

// Header is the parsed ASCII header block of a .bsl file. It declares the
// layer definitions (Y axis) and the byte boundaries of the file regions:
//
//	[0, HeaderBlockSize)                 ASCII header block
//	[CommitLogStart, CommitLogEnd)       human-readable commit log region
//	[DataStart, DataEnd)                 slotted binary data pages
//	[IndexStart, IndexEnd)               page directory, id index, secondary indexes
//	[JournalStart, EOF)                  append-only delta journal
//
// Files written before the index region existed omit IndexStart and
// IndexEnd. Those parse as an empty region sitting on DataEnd, and the
// journal still begins at JournalStart.
type Header struct {
	Version        string
	Format         string
	Layers         map[int]Layer // keyed by layer height (Y axis)
	Branches       []string
	CommitLogStart uint64
	CommitLogEnd   uint64
	DataStart      uint64
	DataEnd        uint64
	IndexStart     uint64
	IndexEnd       uint64
	JournalStart   uint64
}

// ParseHeader parses the ASCII header block of a .bsl file. The block must
// begin with the magic signature and declare every mandatory key.
func ParseHeader(block []byte) (*Header, error) {
	if len(block) == 0 {
		return nil, fmt.Errorf("%w: empty header block", ErrCorruptHeader)
	}
	if len(block) > HeaderBlockSize {
		block = block[:HeaderBlockSize]
	}
	// Cut at NUL padding if the block was zero-filled.
	if i := bytes.IndexByte(block, 0); i >= 0 {
		block = block[:i]
	}
	lines := strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n")

	h := &Header{Layers: map[int]Layer{}}
	seen := map[string]bool{}
	first := true
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if first {
			first = false
			if line != MagicSignature {
				return nil, fmt.Errorf("%w: got %q, want %q", ErrBadMagic, line, MagicSignature)
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%w: malformed header line %q", ErrCorruptHeader, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "Version":
			h.Version = value
		case "Format":
			h.Format = value
		case "Layers":
			layers, err := parseLayers(value)
			if err != nil {
				return nil, err
			}
			h.Layers = layers
		case "Branches":
			if value != "" {
				for _, b := range strings.Split(value, ",") {
					if b = strings.TrimSpace(b); b != "" {
						h.Branches = append(h.Branches, b)
					}
				}
			}
		case "CommitLogStart":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad CommitLogStart %q", ErrCorruptHeader, value)
			}
			h.CommitLogStart = n
		case "CommitLogEnd":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad CommitLogEnd %q", ErrCorruptHeader, value)
			}
			h.CommitLogEnd = n
		case "DataStart":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad DataStart %q", ErrCorruptHeader, value)
			}
			h.DataStart = n
		case "DataEnd":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad DataEnd %q", ErrCorruptHeader, value)
			}
			h.DataEnd = n
		case "JournalStart":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad JournalStart %q", ErrCorruptHeader, value)
			}
			h.JournalStart = n
		case "IndexStart":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad IndexStart %q", ErrCorruptHeader, value)
			}
			h.IndexStart = n
			seen["IndexStart"] = true
		case "IndexEnd":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: bad IndexEnd %q", ErrCorruptHeader, value)
			}
			h.IndexEnd = n
			seen["IndexEnd"] = true
		default:
			// Unknown keys are ignored for forward compatibility.
			continue
		}
		seen[key] = true
	}
	if first {
		return nil, fmt.Errorf("%w: no header lines", ErrCorruptHeader)
	}
	for _, req := range []string{"Version", "Format", "Layers", "CommitLogStart", "CommitLogEnd", "DataStart", "DataEnd", "JournalStart"} {
		if !seen[req] {
			return nil, fmt.Errorf("%w: missing %s", ErrCorruptHeader, req)
		}
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return h, nil
}

// ParseLayers parses a layer specification used by the CLI flags and the
// header's Layers entry list: comma-separated entries of the form
//
//	[Height:]Name[(attr,attr,...)]
//
// Height defaults to the entry position. Examples:
//
//	0:Accounts(name,date),1:Teams(name),2:Users(name,email)
//	Accounts,Teams,Users
func ParseLayers(spec string) ([]Layer, error) {
	var out []Layer
	for i, part := range splitTopLevel(spec, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		height, def := i, part
		if hStr, rest, ok := strings.Cut(part, ":"); ok {
			h, err := strconv.Atoi(strings.TrimSpace(hStr))
			if err != nil {
				return nil, fmt.Errorf("engine: bad layer definition %q", part)
			}
			height, def = h, rest
		}
		name, attrs, indexed, err := parseLayerDef(def)
		if err != nil {
			return nil, fmt.Errorf("engine: bad layer definition %q: %w", part, err)
		}
		if name == "" {
			return nil, fmt.Errorf("engine: empty layer name for height %d", height)
		}
		for _, l := range out {
			if l.Height == height {
				return nil, fmt.Errorf("engine: duplicate layer height %d", height)
			}
		}
		out = append(out, Layer{Height: height, Name: name, Attrs: attrs, Indexed: indexed})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("engine: no layers given")
	}
	return out, nil
}

// splitTopLevel splits s on sep, ignoring separators nested in
// parentheses ("0:Accounts(name,date)" stays one entry).
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// parseLayerDef splits "Accounts(name,date)" into its name and declared
// attribute names. A trailing '!' (url!) marks a hash index and is stored
// separately; the attribute name itself does not include the bang.
func parseLayerDef(def string) (string, []string, []string, error) {
	def = strings.TrimSpace(def)
	open := strings.IndexByte(def, '(')
	if open < 0 {
		return def, nil, nil, nil
	}
	if !strings.HasSuffix(def, ")") {
		return "", nil, nil, fmt.Errorf("unterminated attribute list")
	}
	name := strings.TrimSpace(def[:open])
	var attrs, indexed []string
	for _, a := range splitTopLevel(def[open+1:len(def)-1], ',') {
		a = strings.TrimSpace(a)
		if a == "" {
			return "", nil, nil, fmt.Errorf("empty attribute name")
		}
		if strings.HasSuffix(a, "!") {
			a = strings.TrimSpace(strings.TrimSuffix(a, "!"))
			if a == "" {
				return "", nil, nil, fmt.Errorf("empty attribute name")
			}
			indexed = append(indexed, a)
		}
		attrs = append(attrs, a)
	}
	return name, attrs, indexed, nil
}

// parseLayers parses "​[0:Accounts(name,date), 1:Teams]​" into a map
// keyed by layer height (Y axis).
func parseLayers(value string) (map[int]Layer, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, fmt.Errorf("%w: malformed Layers %q", ErrCorruptHeader, value)
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	layers := map[int]Layer{}
	if inner == "" {
		return layers, nil
	}
	for _, part := range splitTopLevel(inner, ',') {
		part = strings.TrimSpace(part)
		heightStr, def, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("%w: malformed layer entry %q", ErrCorruptHeader, part)
		}
		height, err := strconv.Atoi(strings.TrimSpace(heightStr))
		if err != nil {
			return nil, fmt.Errorf("%w: malformed layer height %q", ErrCorruptHeader, heightStr)
		}
		name, attrs, indexed, err := parseLayerDef(def)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed layer entry %q: %v", ErrCorruptHeader, part, err)
		}
		if name == "" {
			return nil, fmt.Errorf("%w: empty layer name for height %d", ErrCorruptHeader, height)
		}
		if _, dup := layers[height]; dup {
			return nil, fmt.Errorf("%w: duplicate layer height %d", ErrCorruptHeader, height)
		}
		layers[height] = Layer{Height: height, Name: name, Attrs: attrs, Indexed: indexed}
	}
	return layers, nil
}

// Validate checks internal consistency of a parsed header.
func (h *Header) Validate() error {
	switch h.Version {
	case VersionV10, CurrentVersion:
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedVersion, h.Version)
	}
	if h.Format != FormatMatrixSlottedPage {
		return fmt.Errorf("%w: unknown format %q", ErrCorruptHeader, h.Format)
	}
	if len(h.Layers) == 0 {
		return fmt.Errorf("%w: no layers defined", ErrCorruptHeader)
	}
	for height, l := range h.Layers {
		if height < 0 || l.Height != height || strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("%w: bad layer %d:%q", ErrCorruptHeader, height, l.Name)
		}
		seen := map[string]bool{}
		for _, a := range l.Attrs {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("%w: empty attribute name in layer %q", ErrCorruptHeader, l.Name)
			}
			if seen[strings.ToLower(a)] {
				return fmt.Errorf("%w: duplicate attribute %q in layer %q", ErrCorruptHeader, a, l.Name)
			}
			seen[strings.ToLower(a)] = true
		}
		for _, ix := range l.Indexed {
			if _, ok := l.AttrIndex(ix); !ok {
				return fmt.Errorf("%w: indexed attribute %q is not declared on layer %q", ErrCorruptHeader, ix, l.Name)
			}
		}
	}
	// Older files have no index region. Both offsets zero means "absent",
	// which is an empty region at the end of the data pages.
	if h.IndexStart == 0 && h.IndexEnd == 0 {
		h.IndexStart = h.DataEnd
		h.IndexEnd = h.DataEnd
	}
	if h.CommitLogStart < HeaderBlockSize ||
		h.CommitLogEnd < h.CommitLogStart ||
		h.DataStart < h.CommitLogEnd ||
		h.DataEnd < h.DataStart ||
		h.IndexStart < h.DataEnd ||
		h.IndexEnd < h.IndexStart ||
		h.JournalStart < h.IndexEnd {
		return fmt.Errorf("%w: inconsistent region offsets (log %d-%d, data %d-%d, index %d-%d, journal %d)",
			ErrCorruptHeader, h.CommitLogStart, h.CommitLogEnd, h.DataStart, h.DataEnd, h.IndexStart, h.IndexEnd, h.JournalStart)
	}
	return nil
}

// Marshal renders the header as a fixed-size ASCII block (HeaderBlockSize
// bytes, newline padded).
func (h *Header) Marshal() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(MagicSignature + "\n")
	b.WriteString("Version:" + h.Version + "\n")
	b.WriteString("Format:" + h.Format + "\n")

	heights := h.LayerHeights()
	parts := make([]string, 0, len(heights))
	for _, height := range heights {
		l := h.Layers[height]
		entry := fmt.Sprintf("%d:%s", height, l.Name)
		if len(l.Attrs) > 0 {
			entry += "(" + l.headerAttrs() + ")"
		}
		parts = append(parts, entry)
	}
	b.WriteString("Layers:[" + strings.Join(parts, ", ") + "]\n")
	if len(h.Branches) > 0 {
		b.WriteString("Branches:" + strings.Join(h.Branches, ",") + "\n")
	}
	fmt.Fprintf(&b, "CommitLogStart:%d\n", h.CommitLogStart)
	fmt.Fprintf(&b, "CommitLogEnd:%d\n", h.CommitLogEnd)
	fmt.Fprintf(&b, "DataStart:%d\n", h.DataStart)
	fmt.Fprintf(&b, "DataEnd:%d\n", h.DataEnd)
	fmt.Fprintf(&b, "IndexStart:%d\n", h.IndexStart)
	fmt.Fprintf(&b, "IndexEnd:%d\n", h.IndexEnd)
	fmt.Fprintf(&b, "JournalStart:%d\n", h.JournalStart)

	out := []byte(b.String())
	if len(out) > HeaderBlockSize {
		return nil, fmt.Errorf("%w: header exceeds %d bytes", ErrCorruptHeader, HeaderBlockSize)
	}
	padded := make([]byte, HeaderBlockSize)
	copy(padded, out)
	for i := len(out); i < HeaderBlockSize; i++ {
		padded[i] = '\n'
	}
	return padded, nil
}

// LayerHeights returns the layer heights in ascending (topological) order.
func (h *Header) LayerHeights() []int {
	heights := make([]int, 0, len(h.Layers))
	for height := range h.Layers {
		heights = append(heights, height)
	}
	sort.Ints(heights)
	return heights
}

// LayerByName resolves a layer name to its definition.
func (h *Header) LayerByName(name string) (Layer, bool) {
	for _, l := range h.Layers {
		if l.Name == name {
			return l, true
		}
	}
	return Layer{}, false
}

// layerName returns the name of a layer height ("" when unknown).
func (h *Header) layerName(height int) string {
	if l, ok := h.Layers[height]; ok {
		return l.Name
	}
	return ""
}
