package watch

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeZfsPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", "/mnt/tank/photos/2019/IMG_4412.JPG", "/mnt/tank/photos/2019/IMG_4412.JPG"},
		{"space", "/a/From the old box/b.jpg", `/a/From\0040the\0040old\0040box/b.jpg`},
		{"backslash", `/a/b\c`, `/a/b\0134c`},
		{"tab", "/a/b\tc", `/a/b\0011c`},
		{"e with accent", "/a/café.jpg", `/a/caf\0303\0251.jpg`},
		{"del", "/a/b\x7fc", `/a/b\0177c`},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EncodeZfsPath([]byte(c.in)); got != c.want {
				t.Errorf("EncodeZfsPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDecodeZfsPath(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		want      string
		fails     bool
		reason    string
		roundTrip bool // should the round trip succeed?
	}{
		{name: "plain path", in: "/mnt/tank/photos/a.jpg", want: "/mnt/tank/photos/a.jpg", roundTrip: true},
		{name: "space", in: `/a/From\0040the\0040old\0040box/b.jpg`, want: "/a/From the old box/b.jpg", roundTrip: true},
		{name: "backslash", in: `/a/b\0134c`, want: `/a/b\c`, roundTrip: true},
		{name: "tab", in: `/a/b\0011c`, want: "/a/b\tc"}, // decodes, but is a control character
		{name: "e with accent", in: `/a/caf\0303\0251.jpg`, want: "/a/café.jpg", roundTrip: true},
		{name: "brackets and space", in: `/a/PICT0033\0040(5).JPG`, want: "/a/PICT0033 (5).JPG", roundTrip: true},
		{name: "three digits", in: `/a/b\011c`, fails: true, reason: "not an octal digit"},
		{name: "nul byte", in: `/a/b\0000c`, fails: true, reason: "NUL byte"},
		{name: "digit eight", in: `/a/b\0089`, fails: true, reason: "not an octal digit"},
		{name: "value too large", in: `/a/b\0400c`, fails: true, reason: "larger than one byte"},
		{name: "trailing backslash", in: `/a/b\`, fails: true, reason: "four octal digits"},
		{name: "empty", in: "", want: "", roundTrip: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := DecodeZfsPath(c.in)
			if c.fails {
				if err == nil {
					t.Fatalf("DecodeZfsPath(%q) = %q, want an error (%s)", c.in, b, c.reason)
				}
				if !errors.Is(err, ErrZfsPath) {
					t.Errorf("error %v is not an ErrZfsPath", err)
				}
				if _, ok := RoundTripZfsPath(c.in); ok {
					t.Errorf("the round trip succeeded on %q while decoding fails", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeZfsPath(%q) returned an unexpected error: %v", c.in, err)
			}
			if string(b) != c.want {
				t.Errorf("DecodeZfsPath(%q) = %q, want %q", c.in, b, c.want)
			}
			path, ok := RoundTripZfsPath(c.in)
			if ok != c.roundTrip {
				t.Errorf("round trip on %q = %v, want %v (path %q)", c.in, ok, c.roundTrip, path)
			}
			if ok && path != c.want {
				t.Errorf("round trip on %q gave %q, want %q", c.in, path, c.want)
			}
		})
	}
}

// A path containing a literal space cannot come from `zfs diff`: it would have
// written it as \0040. Decoding does succeed (there is no backslash in the
// way), but the round trip spots the difference and rejects it. This is exactly
// the safeguard that keeps an unexpected format from silently producing a wrong
// path.
func TestRoundTripRejectsWhatZfsWouldNeverWrite(t *testing.T) {
	for _, raw := range []string{"/a/with space.jpg", "/a/tab\there.jpg", "/a/\x7f.jpg"} {
		if path, ok := RoundTripZfsPath(raw); ok {
			t.Errorf("round trip on %q succeeded with %q; zfs diff writes that byte escaped", raw, path)
		}
	}
}

// After decoding a path can contain control characters; those must not end up
// in a log line, a message or the report.
func TestRoundTripRejectsControlChars(t *testing.T) {
	// \0012 is a newline: once decoded this path could split one log line in
	// two.
	if path, ok := RoundTripZfsPath(`/a/line\0012end.jpg`); ok {
		t.Errorf("a path with a newline was found decodable: %q", path)
	}
	if path, ok := RoundTripZfsPath(`/a/color\0033[31m.jpg`); ok {
		t.Errorf("a path with an ANSI escape was found decodable: %q", path)
	}
}

func TestContainsControlChar(t *testing.T) {
	if containsControlChar("/a/café (5).jpg") {
		t.Error("a plain path with accents and brackets was seen as a control character")
	}
	// A lone 0xFF is invalid UTF-8 but a valid byte in a file name on Linux;
	// that is not a control character.
	if containsControlChar("/a/" + string([]byte{0xff}) + ".jpg") {
		t.Error("an invalid UTF-8 byte was seen as a control character")
	}
	if !containsControlChar("/a/\n.jpg") {
		t.Error("a newline was not seen as a control character")
	}
}

// FuzzZfsPathRoundTrip checks the property everything rests on: for arbitrary
// bytes, Decode(Encode(b)) must give back exactly b. A NUL byte is the only
// exception, and the decoder rejects it on purpose.
func FuzzZfsPathRoundTrip(f *testing.F) {
	f.Add([]byte("/mnt/tank/photos/2019/IMG_4412.JPG"))
	f.Add([]byte("/mnt/tank/photos/From the old box/PICT0033 (5).JPG"))
	f.Add([]byte("/a/café.jpg"))
	f.Add([]byte(`/a/b\c`))
	f.Add([]byte("/a/b\tc\x7f"))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xfe, 0x80})

	f.Fuzz(func(t *testing.T, b []byte) {
		encoded := EncodeZfsPath(b)
		back, err := DecodeZfsPath(encoded)
		if bytes.IndexByte(b, 0) >= 0 {
			if err == nil {
				t.Fatalf("bytes containing a NUL (%q) were decoded anyway", b)
			}
			return
		}
		if err != nil {
			t.Fatalf("DecodeZfsPath(EncodeZfsPath(%q)) = %v", b, err)
		}
		if !bytes.Equal(back, b) {
			t.Fatalf("round trip turned %q into %q (intermediate %q)", b, back, encoded)
		}
	})
}
