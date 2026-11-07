package proto

// Files and images (PROTOCOL.md §16, cap blob1): a file is uploaded as an
// opaque, already encrypted blob tied to one channel, then attached to a
// message, which carries what it takes to read it (File).

// CapBlob is advertised in WELCOME (server-only, §3) when the server
// stores blobs; WELCOME's max_upload then says how large a file may be.
const CapBlob = "blob1"

// Blob ops.
const (
	OpBlobPut = "blob.put"
	OpBlobGet = "blob.get"
)

// Blob and file limits (PROTOCOL.md §16.4).
const (
	// MaxBlobChunk bounds blob.put data and blob.get len.
	MaxBlobChunk = 256 << 10
	// DefaultMaxUpload is the default largest file (plaintext bytes; the
	// server's --max-upload).
	DefaultMaxUpload = 25 << 20
	// MaxFiles is the most files one message attaches.
	MaxFiles = 10
	// MaxThumb bounds the thumbnails of one message's files together.
	MaxThumb = 32 << 10
	// MaxFileName bounds File.Name (bytes).
	MaxFileName = 255
	// MaxMime bounds File.Mime (bytes).
	MaxMime = 127
	// MaxPendingBlobs is the most blobs one user may hold that aren't
	// attached to a message yet (uploading or uploaded).
	MaxPendingBlobs = 20
	// BlobIDSize is the size of a blob id.
	BlobIDSize = 16
)

// File describes one attached file. In a plaintext channel Message.Files
// carries all of it; in an encrypted one only Blob, and the rest travels
// in the sealed Plain (§16.2), so the server sees the blob and its size.
type File struct {
	Blob []byte `msgpack:"blob"` // 16 bytes, the blob holding the sealed file
	Name string `msgpack:"name,omitempty"`
	// Size is the plaintext size; the blob is e2ecrypto.FileSealedSize(Size).
	Size uint64 `msgpack:"size,omitempty"`
	Mime string `msgpack:"mime,omitempty"`
	Key  []byte `msgpack:"key,omitempty"`  // 32-byte file key (§16.3)
	Hash []byte `msgpack:"hash,omitempty"` // SHA-256 of the plaintext
	// Thumb is a small JPEG preview of an image (≤ MaxThumb with the
	// message's other thumbnails); W and H the image's size in pixels.
	Thumb []byte `msgpack:"thumb,omitempty"`
	W     uint32 `msgpack:"w,omitempty"`
	H     uint32 `msgpack:"h,omitempty"`
}

// BlobPutArgs uploads a chunk of a blob. The first put of an unknown Blob
// creates it (with off 0); each put writes Data at Off only if Off is
// where the blob ends so far, and the result says where that is now, so a
// client resumes from Have after a reconnect.
type BlobPutArgs struct {
	Blob []byte `msgpack:"blob"`
	Chan uint64 `msgpack:"chan"`
	Size uint64 `msgpack:"size"` // the blob's total size
	Off  uint64 `msgpack:"off"`
	Data []byte `msgpack:"data"`
}

type BlobPutRes struct {
	Have uint64 `msgpack:"have"`
}

// BlobGetArgs reads up to Len bytes (default and at most MaxBlobChunk) of
// an attached blob from Off.
type BlobGetArgs struct {
	Blob []byte `msgpack:"blob"`
	Off  uint64 `msgpack:"off"`
	Len  uint32 `msgpack:"len,omitempty"`
}

type BlobGetRes struct {
	Size uint64 `msgpack:"size"`
	Data []byte `msgpack:"data"`
}
