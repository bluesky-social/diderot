package sessiondecode

import "time"

// timeFiller assigns record times for formats whose timestamps are optional
// (ATIF, STS). A record without one takes the latest earlier timestamp; the
// records before the first timestamp take that first one. A document with no
// timestamp at all fails: diderot needs a session's times to publish it.
type timeFiller struct {
	pending []Record
	last    time.Time
	out     *emitter
}

func (f *timeFiller) add(rec Record, at time.Time, ok bool) error {
	if ok {
		f.last = at
		rec.Time, rec.Timestamp = at, at.UTC().Format(time.RFC3339Nano)
		for _, p := range f.pending {
			p.Time, p.Timestamp = at, rec.Timestamp
			if err := f.out.add(p); err != nil {
				return err
			}
		}
		f.pending = nil
		return f.out.add(rec)
	}
	if f.last.IsZero() {
		f.pending = append(f.pending, rec)
		return nil
	}
	rec.Time, rec.Timestamp = f.last, f.last.UTC().Format(time.RFC3339Nano)
	return f.out.add(rec)
}

// mark records a timestamp seen on an entry that emitted no records.
func (f *timeFiller) mark(at time.Time) error {
	f.last = at
	stamp := at.UTC().Format(time.RFC3339Nano)
	for _, p := range f.pending {
		p.Time, p.Timestamp = at, stamp
		if err := f.out.add(p); err != nil {
			return err
		}
	}
	f.pending = nil
	return nil
}

func (f *timeFiller) finish() error {
	if len(f.pending) > 0 {
		return structural("document has no timestamps")
	}
	return f.out.finish()
}
