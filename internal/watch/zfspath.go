package watch

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The escape format of `zfs diff` — measured, not guessed.
//
// In lib/libzfs/libzfs_diff.c, stream_bytes() writes a byte literally when it
// is greater than space, smaller than \177 (DEL) and not a backslash; every
// other byte becomes a backslash followed by exactly four octal digits
// ("\%04o" on a uint8_t, so always \0000 through \0377). A space is \0040, a
// backslash \0134, a tab \0011. Because the backslash escapes itself there is
// no ambiguity at all, which makes decoding a complete, reversible operation.
//
// The consequence that is easy to miss: *every* non-ASCII byte is escaped too,
// byte by byte. An é (UTF-8 0xC3 0xA9) appears as \0303\0251. In an archive
// with accented folder names that is daily practice, not an edge case.
//
// The alternative to a controlled translation is a human turning \0040 into a
// space by hand, which is demonstrably more error-prone. The safeguard on that
// choice is the round trip: the caller decodes, re-encodes and compares byte
// for byte with the original (see RoundTripZfsPath). If that fails, the path
// counts as not decodable and goes into the report in its raw form.

// ErrZfsPath marks a string that is not in the escape format of `zfs diff`.
var ErrZfsPath = errors.New("not in the escape format of zfs diff")

// EncodeZfsPath turns raw bytes into the form `zfs diff` prints them in.
// Exactly stream_bytes(): literal when c > 0x20 && c != '\\' && c < 0x7f.
func EncodeZfsPath(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c > 0x20 && c != '\\' && c < 0x7f {
			sb.WriteByte(c)
			continue
		}
		fmt.Fprintf(&sb, `\%04o`, c)
	}
	return sb.String()
}

// DecodeZfsPath is the inverse of EncodeZfsPath. It rejects a backslash that is
// not followed by exactly four octal digits, and rejects a result holding a NUL
// byte: a NUL cannot occur in a file name and is also the separator in the list
// file we hand to rsync.
//
// What it does not reject is a byte the encoder itself would never write (a
// literal space, for instance). That is deliberate: the caller's round-trip
// check catches those cases, and it is stricter than any list of exceptions we
// could write down here.
func DecodeZfsPath(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+4 >= len(s) {
			return nil, fmt.Errorf("%w: a \\ at position %d is not followed by four octal digits", ErrZfsPath, i)
		}
		value := 0
		for j := 1; j <= 4; j++ {
			d := s[i+j]
			if d < '0' || d > '7' {
				return nil, fmt.Errorf("%w: %q at position %d is not an octal digit", ErrZfsPath, string(d), i+j)
			}
			value = value*8 + int(d-'0')
		}
		if value > 0xff {
			// \0400 and above can never come out of "\%04o" on a uint8_t.
			return nil, fmt.Errorf("%w: the octal value at position %d is larger than one byte", ErrZfsPath, i)
		}
		if value == 0 {
			return nil, fmt.Errorf("%w: a NUL byte cannot occur in a file name", ErrZfsPath)
		}
		out = append(out, byte(value))
		i += 5
	}
	return out, nil
}

// RoundTripZfsPath is the only way the rest of the program may convert a path
// from `zfs diff`: decode, re-encode and compare byte for byte with the
// original. That way an unexpected format can never lead to a wrong path, only
// to a path that has to be checked by hand.
//
// A decoded path containing a control character also counts as not decodable.
// That is a new risk of decoding itself: as long as the paths stayed escaped
// they could by definition not contain a newline or an ANSI escape, and after
// decoding they can. Such paths therefore stay out of the restore script and
// out of the report as text.
func RoundTripZfsPath(raw string) (string, bool) {
	b, err := DecodeZfsPath(raw)
	if err != nil {
		return "", false
	}
	if EncodeZfsPath(b) != raw {
		return "", false
	}
	path := string(b)
	if containsControlChar(path) {
		return "", false
	}
	return path, true
}

// containsControlChar reports whether the string holds a control character:
// anything unicode.IsControl is true for. Those are the ASCII characters below
// space, DEL, and the C1 range (U+0080 through U+009F) as far as it is encoded
// as valid UTF-8 — a CSI written as UTF-8 (0xC2 0x9B) is therefore recognized.
//
// What deliberately does not count, and this is exactly the spot where a reader
// looks for the guarantee: a lone byte from 0x80–0x9F. That is not valid UTF-8,
// utf8.DecodeRuneInString returns RuneError with n == 1 for it, and the loop
// skips it. That is on purpose: file names on Linux are bytes and need not be
// valid UTF-8, and a latin-1 name like "café" would otherwise count as
// untrustworthy in its entirety — no thumbnail, and no line in the restore
// script, while there is nothing wrong with it.
//
// That is acceptable because there is a second safeguard behind it that does
// work on bytes: every path that appears as text goes through pathText or
// cleanText, and those use strings.Map, which replaces every invalid UTF-8 byte
// with U+FFFD. A lone 0x9B therefore never reaches the report, the payload or a
// log line.
func containsControlChar(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			i++
			continue
		}
		if unicode.IsControl(r) {
			return true
		}
		i += n
	}
	return false
}
