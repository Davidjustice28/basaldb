package engine

import (
	"encoding/binary"
	"fmt"
)

// The delta journal is the append-only region after DataEnd. Each record
// captures one commit (metadata + mutations) or one branch event since the
// last checkpoint, framed as:
//
//	u32 magic "JLOG" | u32 payloadLen | payload
const (
	journalMagic  uint32 = 0x4A4C4F47 // "JLOG"
	journalCommit byte   = 1          // v1.0 tuples: uint16 lengths
	journalBranch byte   = 2
	// journalCommitWide is a v1.1 commit frame. Attribute and child-list
	// lengths are uint32, so a long value cannot truncate on the way to
	// disk (issue #44). Kind 1 frames stay readable.
	journalCommitWide byte = 3
)

type journalRecord struct {
	commit *Commit
	branch *BranchEvent
	ops    []Op
}

func marshalJournalCommit(c *Commit, ops []Op) ([]byte, error) {
	opsBytes, err := marshalOps(ops, true)
	if err != nil {
		return nil, err
	}
	payload := []byte{journalCommitWide}
	payload = appendU32Bytes(payload, []byte(c.Text()))
	payload = appendU32Bytes(payload, opsBytes)
	return frameJournal(payload), nil
}

// marshalJournalCommitV10 writes a v1.0 commit frame. Tests use it to
// build a file this engine must still open.
func marshalJournalCommitV10(c *Commit, ops []Op) ([]byte, error) {
	opsBytes, err := marshalOps(ops, false)
	if err != nil {
		return nil, err
	}
	payload := []byte{journalCommit}
	payload = appendU32Bytes(payload, []byte(c.Text()))
	payload = appendU32Bytes(payload, opsBytes)
	return frameJournal(payload), nil
}

func marshalJournalBranch(ev *BranchEvent) []byte {
	payload := []byte{journalBranch}
	payload = appendU16Bytes(payload, []byte(ev.Name))
	payload = appendU16Bytes(payload, []byte(ev.Fork))
	payload = appendU32Bytes(payload, []byte(ev.Text()))
	return frameJournal(payload)
}

func frameJournal(payload []byte) []byte {
	out := make([]byte, 0, 8+len(payload))
	var tmp [8]byte
	binary.LittleEndian.PutUint32(tmp[0:4], journalMagic)
	binary.LittleEndian.PutUint32(tmp[4:8], uint32(len(payload)))
	out = append(out, tmp[:]...)
	out = append(out, payload...)
	return out
}

func appendU16Bytes(dst, b []byte) []byte {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], uint16(len(b)))
	dst = append(dst, tmp[:]...)
	return append(dst, b...)
}

func appendU32Bytes(dst, b []byte) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(b)))
	dst = append(dst, tmp[:]...)
	return append(dst, b...)
}

// marshalOps encodes mutation ops:
//
//	opCreate/opUpdate: u8 kind | i32 layer | record tuple
//	opDelete:          u8 kind | i32 layer | u64 id
func marshalOps(ops []Op, wide bool) ([]byte, error) {
	var b []byte
	for _, op := range ops {
		b = append(b, op.Kind)
		var tmp [8]byte
		binary.LittleEndian.PutUint32(tmp[0:4], uint32(int32(op.Layer)))
		b = append(b, tmp[0:4]...)
		switch op.Kind {
		case opCreate, opUpdate:
			rec, err := encodeRecord(op.Record, wide)
			if err != nil {
				return nil, err
			}
			b = appendU32Bytes(b, rec)
		case opDelete:
			binary.LittleEndian.PutUint64(tmp[:], op.ID)
			b = append(b, tmp[:]...)
		}
	}
	return b, nil
}

func parseOps(b []byte, wide bool) ([]Op, error) {
	var ops []Op
	for off := 0; off < len(b); {
		kind := b[off]
		off++
		if len(b)-off < 4 {
			return nil, fmt.Errorf("engine: truncated journal op")
		}
		layer := int(int32(binary.LittleEndian.Uint32(b[off:])))
		off += 4
		op := Op{Kind: kind, Layer: layer}
		switch kind {
		case opCreate, opUpdate:
			if len(b)-off < 4 {
				return nil, fmt.Errorf("engine: truncated journal record op")
			}
			n := int(binary.LittleEndian.Uint32(b[off:]))
			off += 4
			if n <= 0 || len(b)-off < n {
				return nil, fmt.Errorf("engine: truncated journal record payload")
			}
			rec, err := decodeRecord(b[off:off+n], wide)
			if err != nil {
				return nil, err
			}
			off += n
			op.Record = rec
		case opDelete:
			if len(b)-off < 8 {
				return nil, fmt.Errorf("engine: truncated journal delete op")
			}
			op.ID = binary.LittleEndian.Uint64(b[off:])
			off += 8
		default:
			return nil, fmt.Errorf("engine: unknown journal op kind %d", kind)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// parseJournal decodes the whole delta journal region in append order.
func parseJournal(data []byte) ([]journalRecord, error) {
	var out []journalRecord
	for off := 0; off < len(data); {
		if len(data)-off < 8 {
			return nil, fmt.Errorf("engine: truncated journal frame header")
		}
		if binary.LittleEndian.Uint32(data[off:]) != journalMagic {
			return nil, fmt.Errorf("engine: bad journal magic at offset %d", off)
		}
		plen := int(binary.LittleEndian.Uint32(data[off+4:]))
		off += 8
		if plen <= 0 || len(data)-off < plen {
			return nil, fmt.Errorf("engine: truncated journal frame payload (%d bytes)", plen)
		}
		payload := data[off : off+plen]
		off += plen

		rec := journalRecord{}
		switch payload[0] {
		case journalCommit, journalCommitWide:
			p := payload[1:]
			text, rest, err := takeU32Bytes(p)
			if err != nil {
				return nil, err
			}
			entries, err := ParseLogRegion(text)
			if err != nil {
				return nil, err
			}
			if len(entries) != 1 || entries[0].Commit == nil {
				return nil, fmt.Errorf("engine: journal commit frame carries %d log blocks", len(entries))
			}
			rec.commit = entries[0].Commit
			opsBytes, _, err := takeU32Bytes(rest)
			if err != nil {
				return nil, err
			}
			ops, err := parseOps(opsBytes, payload[0] == journalCommitWide)
			if err != nil {
				return nil, err
			}
			rec.ops = ops
		case journalBranch:
			p := payload[1:]
			name, rest, err := takeU16Bytes(p)
			if err != nil {
				return nil, err
			}
			fork, rest, err := takeU16Bytes(rest)
			if err != nil {
				return nil, err
			}
			text, _, err := takeU32Bytes(rest)
			if err != nil {
				return nil, err
			}
			entries, err := ParseLogRegion(text)
			if err != nil {
				return nil, err
			}
			if len(entries) != 1 || entries[0].Branch == nil {
				return nil, fmt.Errorf("engine: journal branch frame carries %d log blocks", len(entries))
			}
			ev := entries[0].Branch
			if string(name) != ev.Name || string(fork) != ev.Fork {
				return nil, fmt.Errorf("engine: journal branch frame tag mismatch (%q/%q vs %q/%q)",
					name, fork, ev.Name, ev.Fork)
			}
			rec.branch = ev
		default:
			return nil, fmt.Errorf("engine: unknown journal record kind %d", payload[0])
		}
		out = append(out, rec)
	}
	return out, nil
}

func takeU16Bytes(b []byte) ([]byte, []byte, error) {
	if len(b) < 2 {
		return nil, nil, fmt.Errorf("engine: truncated length-prefixed field")
	}
	n := int(binary.LittleEndian.Uint16(b))
	if len(b)-2 < n {
		return nil, nil, fmt.Errorf("engine: truncated length-prefixed field")
	}
	return b[2 : 2+n], b[2+n:], nil
}

func takeU32Bytes(b []byte) ([]byte, []byte, error) {
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("engine: truncated length-prefixed field")
	}
	n := int(binary.LittleEndian.Uint32(b))
	if n < 0 || len(b)-4 < n {
		return nil, nil, fmt.Errorf("engine: truncated length-prefixed field")
	}
	return b[4 : 4+n], b[4+n:], nil
}
