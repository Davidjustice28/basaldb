package engine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
)

// GenerateFlat writes a checkpointed one-layer database of about target
// bytes and returns how many records it contains. Pages and id pairs are
// streamed, so the process does not retain the records. The file is what
// Open reads: a header, slotted pages, and a mapped id index.
func GenerateFlat(path string, target int64) (int, error) {
	if target < 1 {
		return 0, fmt.Errorf("engine: generate target must be positive")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Write(make([]byte, HeaderBlockSize)); err != nil {
		os.Remove(path)
		return 0, err
	}

	ids, err := os.CreateTemp("", "basal-ids-")
	if err != nil {
		os.Remove(path)
		return 0, err
	}
	defer os.Remove(ids.Name())
	defer ids.Close()
	idw := bufio.NewWriterSize(ids, 1<<20)

	const branch = "main"
	nameLen := make([]byte, 2)
	binary.LittleEndian.PutUint16(nameLen, uint16(len(branch)))
	blockLen := make([]byte, 4)
	pair := make([]byte, 16)
	off := uint64(HeaderBlockSize)
	var refs []pageLoc
	var records int
	page := NewPage(0, 0)
	flush := func() error {
		pageBytes, err := page.MarshalBinary()
		if err != nil {
			return err
		}
		payloadLen := 2 + len(branch) + len(pageBytes)
		binary.LittleEndian.PutUint32(blockLen, uint32(payloadLen))
		if _, err := f.Write(blockLen); err != nil {
			return err
		}
		if _, err := f.Write(nameLen); err != nil {
			return err
		}
		if _, err := f.WriteString(branch); err != nil {
			return err
		}
		if _, err := f.Write(pageBytes); err != nil {
			return err
		}
		refs = append(refs, pageLoc{off: off, n: uint32(payloadLen)})
		off += 4 + uint64(payloadLen)
		return nil
	}

	// A short value keeps the record small so a multi-gigabyte file still
	// has an id index large enough to matter at open.
	attr := "user-value"
	for int64(off-HeaderBlockSize) < target {
		slot := uint64(records)
		if records > 0 && slot%SlotsPerPage == 0 {
			if err := flush(); err != nil {
				os.Remove(path)
				return 0, err
			}
			page = NewPage(0, slot)
		}
		id := slot + 1
		if err := page.Put(&Record{ID: id, Slot: slot, Attrs: []string{attr}}); err != nil {
			os.Remove(path)
			return 0, err
		}
		binary.LittleEndian.PutUint64(pair[:8], id)
		binary.LittleEndian.PutUint64(pair[8:], slot)
		if _, err := idw.Write(pair); err != nil {
			os.Remove(path)
			return 0, err
		}
		records++
	}
	if records == 0 {
		os.Remove(path)
		return 0, fmt.Errorf("engine: generate produced no records")
	}
	if err := flush(); err != nil {
		os.Remove(path)
		return 0, err
	}
	if err := idw.Flush(); err != nil {
		os.Remove(path)
		return 0, err
	}
	if _, err := ids.Seek(0, 0); err != nil {
		os.Remove(path)
		return 0, err
	}

	dataEnd := off
	lm := &layerMeta{
		height:   0,
		nextSlot: uint64(records),
		count:    records,
		refs:     map[uint64]pageLoc{},
		idCount:  uint64(records),
		idRead:   ids,
	}
	for i, loc := range refs {
		lm.refs[uint64(i)] = loc
	}
	metas := map[string]map[int]*layerMeta{
		branch: {0: lm},
	}
	indexN, err := writeIndex(f, []string{branch}, metas)
	if err != nil {
		os.Remove(path)
		return 0, err
	}
	indexEnd := dataEnd + uint64(indexN)
	hdr := &Header{
		Version:        CurrentVersion,
		Format:         FormatMatrixSlottedPage,
		Layers:         map[int]Layer{0: {Height: 0, Name: "Users", Attrs: []string{"name"}}},
		Branches:       []string{branch},
		CommitLogStart: HeaderBlockSize,
		CommitLogEnd:   HeaderBlockSize,
		DataStart:      HeaderBlockSize,
		DataEnd:        dataEnd,
		IndexStart:     dataEnd,
		IndexEnd:       indexEnd,
		JournalStart:   indexEnd,
	}
	block, err := hdr.Marshal()
	if err != nil {
		os.Remove(path)
		return 0, err
	}
	if _, err := f.WriteAt(block, 0); err != nil {
		os.Remove(path)
		return 0, err
	}
	if err := f.Sync(); err != nil {
		os.Remove(path)
		return 0, err
	}
	return records, nil
}
