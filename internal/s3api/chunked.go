package s3api

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hpolthof/webdavs3/internal/auth"
)

// awsChunkedReader decodes an AWS SigV4 streaming (aws-chunked) request body
// and verifies per-chunk signatures. The reader buffers a single chunk at a
// time so the chunk hash can be computed and verified before any data is
// returned to the caller.
type awsChunkedReader struct {
	r               *bufio.Reader
	signingKey      []byte
	seedSignature   string
	prevSignature   string
	amzDate         string
	credentialScope string

	buf       []byte
	off       int
	done      bool
	err       error
	chunkMeta chunkHeader
}

type chunkHeader struct {
	size      int64
	signature string
}

// newAWSChunkedReader wraps r with AWS chunked decoding and optional signature
// verification. signingKey may be nil to skip verification.
func newAWSChunkedReader(r io.Reader, signingKey []byte, seedSignature, amzDate, credentialScope string) *awsChunkedReader {
	return &awsChunkedReader{
		r:               bufio.NewReader(r),
		signingKey:      signingKey,
		seedSignature:   seedSignature,
		prevSignature:   seedSignature,
		amzDate:         amzDate,
		credentialScope: credentialScope,
	}
}

func (cr *awsChunkedReader) Read(p []byte) (int, error) {
	if cr.err != nil {
		return 0, cr.err
	}
	if cr.done && cr.off >= len(cr.buf) {
		return 0, io.EOF
	}

	if cr.off >= len(cr.buf) {
		if err := cr.loadNextChunk(); err != nil {
			cr.err = err
			return 0, err
		}
		if cr.done {
			return 0, io.EOF
		}
	}

	n := copy(p, cr.buf[cr.off:])
	cr.off += n
	return n, nil
}

func (cr *awsChunkedReader) loadNextChunk() error {
	if cr.done {
		return nil
	}

	hdr, err := cr.readChunkHeader()
	if err != nil {
		return err
	}
	cr.chunkMeta = hdr

	if hdr.size == 0 {
		// Final chunk: verify its signature, consume trailing headers, and end.
		if err := cr.verifyChunk([]byte{}); err != nil {
			return err
		}
		if err := cr.readTrailingHeaders(); err != nil {
			return err
		}
		cr.done = true
		return nil
	}

	data := make([]byte, hdr.size)
	if _, err := io.ReadFull(cr.r, data); err != nil {
		return fmt.Errorf("read chunk data: %w", err)
	}
	if err := cr.readCRLF(); err != nil {
		return err
	}
	if err := cr.verifyChunk(data); err != nil {
		return err
	}

	cr.buf = data
	cr.off = 0
	return nil
}

func (cr *awsChunkedReader) readChunkHeader() (chunkHeader, error) {
	line, err := cr.r.ReadString('\n')
	if err != nil {
		return chunkHeader{}, fmt.Errorf("read chunk header: %w", err)
	}
	if !strings.HasSuffix(line, "\r\n") {
		return chunkHeader{}, errors.New("malformed chunk header: missing CRLF")
	}
	line = strings.TrimSuffix(line, "\r\n")

	parts := strings.Split(line, ";")
	if len(parts) < 1 {
		return chunkHeader{}, errors.New("malformed chunk header")
	}
	sizeStr := strings.TrimSpace(parts[0])
	size, err := strconv.ParseInt(sizeStr, 16, 64)
	if err != nil {
		return chunkHeader{}, fmt.Errorf("parse chunk size %q: %w", sizeStr, err)
	}

	var sig string
	for _, ext := range parts[1:] {
		ext = strings.TrimSpace(ext)
		const prefix = "chunk-signature="
		if strings.HasPrefix(ext, prefix) {
			sig = strings.TrimSpace(ext[len(prefix):])
			break
		}
	}
	if size > 0 && sig == "" {
		return chunkHeader{}, errors.New("missing chunk-signature")
	}
	return chunkHeader{size: size, signature: sig}, nil
}

func (cr *awsChunkedReader) readCRLF() error {
	b := make([]byte, 2)
	if _, err := io.ReadFull(cr.r, b); err != nil {
		return fmt.Errorf("read chunk CRLF: %w", err)
	}
	if !bytes.Equal(b, []byte("\r\n")) {
		return errors.New("expected CRLF after chunk data")
	}
	return nil
}

func (cr *awsChunkedReader) readTrailingHeaders() error {
	for {
		line, err := cr.r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read trailing header: %w", err)
		}
		if line == "\r\n" {
			return nil
		}
	}
}

func (cr *awsChunkedReader) verifyChunk(data []byte) error {
	if len(cr.signingKey) == 0 {
		cr.prevSignature = cr.chunkMeta.signature
		return nil
	}
	emptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	dataHash := auth.HashSHA256(data)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		cr.amzDate,
		cr.credentialScope,
		cr.prevSignature,
		emptyHash,
		dataHash,
	}, "\n")
	computed := hex.EncodeToString(auth.HMACSHA256(cr.signingKey, []byte(stringToSign)))
	if !hmac.Equal([]byte(computed), []byte(cr.chunkMeta.signature)) {
		return fmt.Errorf("chunk signature mismatch: computed %s expected %s", computed, cr.chunkMeta.signature)
	}
	cr.prevSignature = cr.chunkMeta.signature
	return nil
}
