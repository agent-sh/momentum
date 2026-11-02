package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agent-sh/momentum/internal/jail"
	"github.com/agent-sh/momentum/internal/source"
	"github.com/agent-sh/momentum/internal/store"
)

// A pair's capture fits one door stream. When full, further records are withheld for both arms.
const captureMax = jail.MaxStream
const captureFull = "  (record withheld: the pair's raw record capture is full)\n"

type captured struct {
	Record, Note, Error string
}

type capture struct {
	Full    bool
	Records map[string]captured
}

// capturedRecord reads a record once per pair, including its unavailable or withheld state. The capture is
// private to the evaluation, outside either sandbox. All evidence show processes use the same file lock.
func capturedRecord(ctx context.Context, dir string, e store.Event) (string, string, error) {
	lock, err := os.OpenFile(filepath.Join(dir, "records.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", "", err
	}
	defer lock.Close()
	// A raw record read already has a ten-second deadline. Do not wait longer for another read's lock.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return "", "", err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-cctx.Done():
			timer.Stop()
			return "", "", cctx.Err()
		case <-timer.C:
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(dir, "records.json")
	cache := capture{Records: map[string]captured{}}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		st, serr := f.Stat()
		if serr != nil || !st.Mode().IsRegular() || st.Size() > captureMax {
			f.Close()
			return "", "", fmt.Errorf("invalid pair record capture")
		}
		raw, rerr := io.ReadAll(io.LimitReader(f, captureMax+1))
		f.Close()
		if rerr != nil {
			return "", "", rerr
		}
		if err := json.Unmarshal(raw, &cache); err != nil || cache.Records == nil {
			return "", "", fmt.Errorf("invalid pair record capture")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	// Several events can name the same row. Parse the place before removing its event index: a file name may hold #.
	ref := e.Ref
	if path, offset, ok := placeOf(ref); ok {
		ref = fmt.Sprintf("%s@%d", path, offset)
	}
	keyedLine := ""
	if e.Source == "mistakes" {
		keyedLine = e.SourceKey // a mistakes row is its event's only while the line's hash matches
	}
	rowVersion := eventMeta(e)[source.HermesDigestKey]
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s", e.Source, ref, e.Session, e.Agent, e.TS.UnixMilli(), rowVersion, keyedLine)))
	key := hex.EncodeToString(sum[:])
	if record, ok := cache.Records[key]; ok {
		return record.result()
	}
	if cache.Full {
		return "", captureFull, nil
	}
	record, note, readErr := readEventRecord(cctx, e)
	entry := captured{Record: record, Note: note}
	if readErr != nil {
		entry.Error = readErr.Error()
	}
	cache.Records[key] = entry
	raw, err := json.Marshal(cache)
	if err != nil {
		return "", "", err
	}
	if len(raw) > captureMax {
		delete(cache.Records, key)
		cache.Full = true // true is shorter than false, so this marker still fits
		entry = captured{Note: captureFull}
		raw, err = json.Marshal(cache)
		if err != nil {
			return "", "", err
		}
	}
	tmp, err := os.CreateTemp(dir, ".records-")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(raw)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", "", err
	}
	return entry.result()
}

func (c captured) result() (string, string, error) {
	if c.Error != "" {
		return "", "", errors.New(c.Error)
	}
	return c.Record, c.Note, nil
}
