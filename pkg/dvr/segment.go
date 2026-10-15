package dvr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	v22 "github.com/MoQ-it/moqt/wire/v22"
)

const magic = "MQDVR1\n"

// record is one stored Object.
type record struct {
	group, object, subgroup uint64
	prio                    uint8
	at                      int64 // server receive time, unix µs
	props                   v22.KeyValues
	payload                 []byte
}

// locProperty is the LOC capture timestamp (µs since the Unix epoch).
const locProperty = 0x10

func (r *record) loc() int64 {
	if kv, ok := r.props.Lookup(locProperty); ok {
		return int64(kv.Int)
	}
	return 0
}

func (r *record) append(b []byte) []byte {
	var props []byte
	if len(r.props) > 0 {
		props = r.props.Append(nil)
	}
	body := v22.AppendVarint(nil, r.group)
	body = v22.AppendVarint(body, r.object)
	body = v22.AppendVarint(body, r.subgroup)
	body = append(body, r.prio)
	body = v22.AppendVarint(body, uint64(r.at))
	body = v22.AppendVarint(body, uint64(len(props)))
	body = append(body, props...)
	body = append(body, r.payload...)
	return append(v22.AppendVarint(b, uint64(len(body))), body...)
}

// maxRecord bounds a record's length when reading.
var maxRecord = v22.MaxObjectPayload + v22.MaxObjectProperties + 64

var errTorn = errors.New("dvr: torn record")

// readRecord reads one record and its size on disk. It returns io.EOF at a
// clean end and errTorn for a record cut short.
func readRecord(br *bufio.Reader) (*record, int64, error) {
	n, err := v22.ReadVarint(br)
	if err == io.EOF {
		return nil, 0, io.EOF
	}
	if err != nil || n > maxRecord {
		return nil, 0, errTorn
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, 0, errTorn
	}
	d := v22.NewDecoder(body)
	r := &record{group: d.Varint(), object: d.Varint(), subgroup: d.Varint(), prio: d.Byte(), at: int64(d.Varint())}
	if pl := d.Varint(); d.Err() == nil && pl > 0 {
		pd := v22.NewDecoder(d.Bytes(pl))
		r.props = pd.KeyValues()
		if pd.Err() != nil {
			return nil, 0, errTorn
		}
	}
	r.payload = d.Rest()
	if d.Err() != nil {
		return nil, 0, errTorn
	}
	return r, int64(len(v22.AppendVarint(nil, n))) + int64(n), nil
}

func writeHeader(w io.Writer, m Meta) (int64, error) {
	js, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	b := append([]byte(magic), v22.AppendVarint(nil, uint64(len(js)))...)
	b = append(b, js...)
	_, err = w.Write(b)
	return int64(len(b)), err
}

func readHeader(br *bufio.Reader) (Meta, int64, error) {
	var m Meta
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(br, head); err != nil || string(head) != magic {
		return m, 0, fmt.Errorf("dvr: not a segment")
	}
	n, err := v22.ReadVarint(br)
	if err != nil || n > 1<<16 {
		return m, 0, fmt.Errorf("dvr: bad segment header")
	}
	js := make([]byte, n)
	if _, err := io.ReadFull(br, js); err != nil {
		return m, 0, err
	}
	if err := json.Unmarshal(js, &m); err != nil {
		return m, 0, err
	}
	return m, int64(len(magic)) + int64(len(v22.AppendVarint(nil, n))) + int64(n), nil
}

// scanSegment rebuilds a segment's index from disk, truncating a torn
// tail left by a crash, and returns the Group starts it holds.
func scanSegment(path string) (Meta, *segment, []groupStart, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return Meta{}, nil, nil, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	m, off, err := readHeader(br)
	if err != nil {
		return m, nil, nil, err
	}
	var starts []groupStart
	seg := &segment{path: path, dataOff: off, size: off}
	for {
		r, n, err := readRecord(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			if terr := f.Truncate(seg.size); terr != nil {
				return m, nil, nil, terr
			}
			break
		}
		seg.add(r)
		if r.object == 0 {
			r.payload = nil // only the position and timestamps are kept
			starts = append(starts, groupStart{r: r, off: seg.size})
		}
		seg.size += n
	}
	return m, seg, starts, nil
}
