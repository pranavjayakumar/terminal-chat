package client

import (
	"bytes"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

func testFile(b byte) proto.File {
	return proto.File{Blob: bytes.Repeat([]byte{b}, 16), Name: "f.png", Size: 3, Mime: "image/png",
		Key: make([]byte, 32), Hash: make([]byte, 32)}
}

// TestEncFilesMatchServer: an encrypted message's files count only when
// they are well-formed and name exactly the blobs the server attached; a
// waiting message shows none, and gets them once it opens.
func TestEncFilesMatchServer(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	c, f, b := d.c, d.f, d.b1.dev
	seal := func(fs ...proto.File) []byte {
		return sealAs(t, b, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "📎 f.png", Files: fs})
	}
	a, x := testFile(1), testFile(2)
	bad := testFile(3)
	bad.Mime = "text/html; charset=utf-8"
	cases := []struct {
		name  string
		enc   []byte
		srv   []proto.File
		files int
	}{
		{"matching", seal(a), blobIDs([]proto.File{a}), 1},
		{"server lists another blob", seal(a), blobIDs([]proto.File{x}), 0},
		{"server lists none", seal(a), nil, 0},
		{"server lists more", seal(a), blobIDs([]proto.File{a, x}), 0},
		{"order differs", seal(a, x), blobIDs([]proto.File{x, a}), 0},
		{"malformed mime", seal(bad), blobIDs([]proto.File{bad}), 0},
		{"server smuggles metadata", seal(), []proto.File{x}, 0},
	}
	ids := make([]uint64, len(cases))
	for i, tc := range cases {
		ids[i] = f.post(proto.Message{Chan: dm, Author: bob, Enc: tc.enc, Files: tc.srv})
	}
	waitFor(t, "applied", func() bool { return lastSeq(t, c) >= ids[len(ids)-1] })
	for i, tc := range cases {
		m, ok := findMsg(c, ids[i])
		if !ok || m.Sec != SecE2E || len(m.Files) != tc.files {
			t.Errorf("%s: sec %s files %d (found %v), want %d", tc.name, m.Sec, len(m.Files), ok, tc.files)
		}
		if tc.files > 0 && (m.Files[0].Name != "f.png" || len(m.Files[0].Key) != 32) {
			t.Errorf("%s: files %+v", tc.name, m.Files)
		}
		if tc.files > 0 && !Downloadable(m, 0) {
			t.Errorf("%s: not downloadable", tc.name)
		}
	}
}

func TestValidFiles(t *testing.T) {
	ok := testFile(1)
	if !validFiles([]proto.File{ok}) {
		t.Fatal("good file refused")
	}
	mut := []func(*proto.File){
		func(f *proto.File) { f.Blob = f.Blob[:15] },
		func(f *proto.File) { f.Key = nil },
		func(f *proto.File) { f.Hash = f.Hash[:31] },
		func(f *proto.File) { f.Name = "" },
		func(f *proto.File) { f.Name = "a\nb" },
		func(f *proto.File) { f.Name = "a\x1b[2Jb" },
		func(f *proto.File) { f.Mime = "image/" },
		func(f *proto.File) { f.Size = MaxDownload + 1 },
		func(f *proto.File) { f.Thumb = make([]byte, proto.MaxThumb+1) },
	}
	for i, m := range mut {
		f := testFile(1)
		m(&f)
		if validFiles([]proto.File{f}) {
			t.Errorf("mutation %d accepted", i)
		}
	}
	if validFiles([]proto.File{ok, ok}) {
		t.Error("same blob twice accepted")
	}
}
