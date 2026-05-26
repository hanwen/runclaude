package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// mountEntry is one row from /proc/self/mountinfo. Field numbers refer to
// proc(5) §3.5. Only the fields we currently use are populated.
type mountEntry struct {
	mountID    int    // field 1
	parentID   int    // field 2
	dev        string // field 3, "major:minor"
	rootInFS   string // field 4, path within the filesystem
	mountPoint string // field 5, mount point in the namespace
}

// parseMountinfo parses the mountinfo format. Octal escapes (\040, \011,
// \012, \134) in fields 4 and 5 are decoded.
func parseMountinfo(r io.Reader) ([]mountEntry, error) {
	var out []mountEntry
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 1<<16), 1<<20)
	for s.Scan() {
		line := s.Text()
		// Fields after field 6 are optional ("shared:N", "master:N", ...)
		// terminated by " - ". Splitting there isolates the leading
		// fixed-position fields we need.
		dash := strings.Index(line, " - ")
		if dash < 0 {
			return nil, fmt.Errorf("mountinfo: missing %q separator: %q", " - ", line)
		}
		head := strings.Fields(line[:dash])
		if len(head) < 5 {
			return nil, fmt.Errorf("mountinfo: too few fields: %q", line)
		}
		mountID, err := strconv.Atoi(head[0])
		if err != nil {
			return nil, fmt.Errorf("mountinfo: mountID %q: %w", head[0], err)
		}
		parentID, err := strconv.Atoi(head[1])
		if err != nil {
			return nil, fmt.Errorf("mountinfo: parentID %q: %w", head[1], err)
		}
		out = append(out, mountEntry{
			mountID:    mountID,
			parentID:   parentID,
			dev:        head[2],
			rootInFS:   unescapeOctal(head[3]),
			mountPoint: unescapeOctal(head[4]),
		})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) &&
			isOctalDigit(s[i+1]) && isOctalDigit(s[i+2]) && isOctalDigit(s[i+3]) {
			b.WriteByte(((s[i+1] - '0') << 6) | ((s[i+2] - '0') << 3) | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctalDigit(b byte) bool { return b >= '0' && b <= '7' }

// pathContains reports whether `child` is at or below `parent` in a rooted
// path hierarchy. Both arguments are absolute paths with no trailing slash
// (except `parent == "/"`, which contains every absolute path).
func pathContains(parent, child string) bool {
	parent = strings.TrimRight(parent, "/")
	if parent == "" {
		return strings.HasPrefix(child, "/")
	}
	return child == parent || strings.HasPrefix(child, parent+"/")
}
