package main

import (
	"strings"
	"testing"
)

func TestParseMountinfo(t *testing.T) {
	in := `5976 5902 252:1 /tmp/runclaude/rootfs / rw,relatime - ext4 /dev/sda1 rw
5988 5987 0:37 / /mnt/data rw,noatime shared:42 - btrfs /dev/dm-0 rw,subvol=/
6057 5976 0:37 /hanwen-cache/rc/abc/home /home/hanwen rw,nosuid,nodev,noatime - btrfs /dev/dm-0 rw,subvol=/hanwen-cache
6058 5976 0:37 /space\040with\040octal /weird\134path rw - btrfs /dev/dm-0 rw
`
	got, err := parseMountinfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d entries, want 4", len(got))
	}
	wants := []mountEntry{
		{5976, 5902, "252:1", "/tmp/runclaude/rootfs", "/"},
		{5988, 5987, "0:37", "/", "/mnt/data"},
		{6057, 5976, "0:37", "/hanwen-cache/rc/abc/home", "/home/hanwen"},
		{6058, 5976, "0:37", "/space with octal", "/weird\\path"},
	}
	for i, w := range wants {
		if got[i] != w {
			t.Errorf("entry %d:\n got=%+v\nwant=%+v", i, got[i], w)
		}
	}
}

func TestParseMountinfoErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"missing dash", "5976 5902 252:1 / / rw,relatime ext4 /dev/sda1 rw"},
		{"too few fields", "5976 5902 - ext4 /dev/sda1 rw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseMountinfo(strings.NewReader(tc.in)); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestUnescapeOctal(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"plain", "plain"},
		{`a\040b`, "a b"},
		{`a\011b`, "a\tb"},
		{`a\012b`, "a\nb"},
		{`a\134b`, `a\b`},
		{`\040`, " "},
		{`partial\04`, `partial\04`}, // not a valid 3-digit escape
		{`badseq\x40`, `badseq\x40`}, // non-octal after \, untouched
		{`mixed\040and\134end`, `mixed and\end`},
	} {
		if got := unescapeOctal(tc.in); got != tc.want {
			t.Errorf("unescapeOctal(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPathContains(t *testing.T) {
	for _, tc := range []struct {
		parent, child string
		want          bool
	}{
		{"/", "/anything", true},
		{"/", "/", true},
		{"/foo", "/foo", true},
		{"/foo", "/foo/bar", true},
		{"/foo", "/foo/", true},
		{"/foo", "/foobar", false},
		{"/foo/bar", "/foo", false},
		{"/foo/", "/foo/bar", true},
		{"", "/x", true},
	} {
		if got := pathContains(tc.parent, tc.child); got != tc.want {
			t.Errorf("pathContains(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}
