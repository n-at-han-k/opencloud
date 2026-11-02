package workflow

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// This file does the small amount of ISO-BMFF box surgery the segment pipeline
// needs. It exists because of one stubborn ffmpeg behaviour, established by
// measurement rather than assumption:
//
// ffmpeg normalizes every output's timeline to start at zero, per invocation.
// Neither -copyts, nor -output_ts_offset, nor -start_at_zero, nor
// -avoid_negative_ts disabled changes it, and the HLS muxer does the same thing
// one level up. So a segment produced by its own ffmpeg run always claims to
// start at t=0, and a player handed three of them stacks them on top of each
// other instead of playing them in sequence.
//
// Independently produced segments are the whole point of the design, so the
// position has to be written in afterwards: take the one number that says where
// a fragment sits — the track fragment decode time — and add the segment's real
// start to it. That is the entire fix, and it is why these two functions exist.

// ErrMalformedMP4 is returned when ffmpeg's output cannot be walked as ISO-BMFF.
var ErrMalformedMP4 = errors.New("hls: malformed mp4 output")

// boxHeaderSize is the size of a plain box header: 4 bytes of size, 4 of type.
const boxHeaderSize = 8

// walkBoxes calls fn for each top-level box in buf with the box's own start, the
// start of its body and its end.
func walkBoxes(buf []byte, fn func(boxType string, boxStart, bodyStart, boxEnd int) error) error {
	off := 0
	for off+boxHeaderSize <= len(buf) {
		size := int(binary.BigEndian.Uint32(buf[off:]))
		boxType := string(buf[off+4 : off+8])
		header := boxHeaderSize

		switch size {
		case 1:
			// 64-bit extended size follows the header.
			if off+16 > len(buf) {
				return fmt.Errorf("%w: truncated large box header", ErrMalformedMP4)
			}
			size64 := binary.BigEndian.Uint64(buf[off+8:])
			if size64 > math.MaxInt32 {
				return fmt.Errorf("%w: box too large", ErrMalformedMP4)
			}
			size = int(size64)
			header = 16
		case 0:
			// A size of zero means the box runs to the end of the file.
			size = len(buf) - off
		}

		if size < header || off+size > len(buf) {
			return fmt.Errorf("%w: box %q has size %d at offset %d of %d",
				ErrMalformedMP4, boxType, size, off, len(buf))
		}

		if err := fn(boxType, off, off+header, off+size); err != nil {
			return err
		}
		off += size
	}

	return nil
}

// findBoxes calls fn for every box of the given type within the bounds.
func findBoxes(buf []byte, start, end int, boxType string, fn func(bodyStart, boxEnd int) error) error {
	return walkBoxes(buf[start:end], func(t string, _, bs, be int) error {
		if t != boxType {
			return nil
		}

		return fn(start+bs, start+be)
	})
}

// splitInitAndMedia splits a fragmented MP4 into its CMAF pair: everything
// before the first moof is the initialization segment (ftyp and moov), and from
// the first moof onward is the media segment.
//
// The split is why the init can be cached once and reused: ffmpeg writes a
// byte-identical init for every segment of a source, because with +empty_moov
// it describes only the codec configuration and carries no per-segment state.
func splitInitAndMedia(buf []byte) (init, media []byte, err error) {
	boundary := -1

	err = walkBoxes(buf, func(boxType string, boxStart, _, _ int) error {
		if boxType == "moof" {
			boundary = boxStart

			return errStopWalk
		}

		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return nil, nil, err
	}

	if boundary <= 0 {
		return nil, nil, fmt.Errorf("%w: no moof box found", ErrMalformedMP4)
	}

	media = buf[boundary:]

	// ffmpeg closes a fragmented file with an mfra box, a random-access index
	// for the whole file. In a segment it describes only that one fragment,
	// which the playlist already says everything about, and it is not part of a
	// CMAF segment. Players skip it, but there is no reason to serve and cache
	// it on every segment.
	if end, ok := mediaEndBeforeMfra(media); ok {
		media = media[:end]
	}

	return buf[:boundary], media, nil
}

// mediaEndBeforeMfra reports where the media data ends when a trailing mfra box
// follows it. It returns false when there is nothing to trim.
func mediaEndBeforeMfra(media []byte) (int, bool) {
	end := -1

	err := walkBoxes(media, func(boxType string, boxStart, _, boxEnd int) error {
		switch boxType {
		case "mfra":
			if end < 0 {
				end = boxStart
			}
		default:
			// Anything after an mfra means this is not a plain trailer, so
			// leave the segment alone rather than cutting real data.
			end = -1
		}

		return nil
	})
	if err != nil || end <= 0 {
		return 0, false
	}

	return end, true
}

// errStopWalk ends a walk early; it never escapes this file.
var errStopWalk = errors.New("stop walk")

// trackTimescales reads each track's timescale out of an initialization
// segment, by track ID. A fragment's decode time is in its track's timescale, so
// shifting it needs these.
func trackTimescales(init []byte) (map[uint32]uint32, error) {
	scales := map[uint32]uint32{}

	err := findBoxes(init, 0, len(init), "moov", func(ms, me int) error {
		return findBoxes(init, ms, me, "trak", func(ts, te int) error {
			var trackID, timescale uint32

			if err := findBoxes(init, ts, te, "tkhd", func(hs, he int) error {
				// tkhd: version(1) flags(3), then creation and modification
				// times, then the track ID. The times are 8 bytes each in
				// version 1 and 4 each in version 0.
				offset := hs + 4
				if init[hs] == 1 {
					offset += 16
				} else {
					offset += 8
				}
				if offset+4 > he {
					return fmt.Errorf("%w: short tkhd", ErrMalformedMP4)
				}
				trackID = binary.BigEndian.Uint32(init[offset:])

				return nil
			}); err != nil {
				return err
			}

			if err := findBoxes(init, ts, te, "mdia", func(ds, de int) error {
				return findBoxes(init, ds, de, "mdhd", func(hs, he int) error {
					offset := hs + 4
					if init[hs] == 1 {
						offset += 16
					} else {
						offset += 8
					}
					if offset+4 > he {
						return fmt.Errorf("%w: short mdhd", ErrMalformedMP4)
					}
					timescale = binary.BigEndian.Uint32(init[offset:])

					return nil
				})
			}); err != nil {
				return err
			}

			if trackID != 0 && timescale != 0 {
				scales[trackID] = timescale
			}

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	if len(scales) == 0 {
		return nil, fmt.Errorf("%w: no track timescales in init segment", ErrMalformedMP4)
	}

	return scales, nil
}

// shiftDecodeTime moves a media segment onto its real place in the timeline, by
// adding start to every track fragment decode time in it. The buffer is edited
// in place; only the tfdt values change, so every data offset in the segment
// stays valid.
func shiftDecodeTime(media []byte, scales map[uint32]uint32, start float64) error {
	if start == 0 {
		return nil
	}

	shifted := 0

	err := findBoxes(media, 0, len(media), "moof", func(ms, me int) error {
		return findBoxes(media, ms, me, "traf", func(ts, te int) error {
			var trackID uint32

			if err := findBoxes(media, ts, te, "tfhd", func(hs, he int) error {
				if hs+8 > he {
					return fmt.Errorf("%w: short tfhd", ErrMalformedMP4)
				}
				trackID = binary.BigEndian.Uint32(media[hs+4:])

				return nil
			}); err != nil {
				return err
			}

			timescale, ok := scales[trackID]
			if !ok {
				// A fragment for a track the init does not describe is not ours
				// to interpret; leaving it alone is safer than guessing a
				// timescale.
				return nil
			}
			delta := uint64(math.Round(start * float64(timescale)))

			return findBoxes(media, ts, te, "tfdt", func(hs, he int) error {
				version := media[hs]
				switch version {
				case 1:
					if hs+12 > he {
						return fmt.Errorf("%w: short 64-bit tfdt", ErrMalformedMP4)
					}
					base := binary.BigEndian.Uint64(media[hs+4:])
					binary.BigEndian.PutUint64(media[hs+4:], base+delta)
				case 0:
					if hs+8 > he {
						return fmt.Errorf("%w: short 32-bit tfdt", ErrMalformedMP4)
					}
					base := uint64(binary.BigEndian.Uint32(media[hs+4:]))
					if base+delta > math.MaxUint32 {
						// Far enough into a long film that a 32-bit decode time
						// cannot hold the result. Rewriting the box as version 1
						// would move every byte after it, so this is reported
						// rather than silently truncated to a wrong timestamp.
						return fmt.Errorf("%w: decode time %d overflows a 32-bit tfdt",
							ErrMalformedMP4, base+delta)
					}
					binary.BigEndian.PutUint32(media[hs+4:], uint32(base+delta))
				default:
					return fmt.Errorf("%w: unknown tfdt version %d", ErrMalformedMP4, version)
				}
				shifted++

				return nil
			})
		})
	})
	if err != nil {
		return err
	}

	if shifted == 0 {
		return fmt.Errorf("%w: no tfdt box to shift", ErrMalformedMP4)
	}

	return nil
}
