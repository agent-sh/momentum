package loop

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
)

const gitBase85 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"

// checkBinary bounds both literal and delta expansion before Git allocates the result.
func checkBinary(lines []string, start int) (int, int64, error) {
	i := start
	var expanded int64
	for block := 0; block < 2; block++ {
		if i >= len(lines) || lines[i] == "" {
			break
		}
		kind, count, ok := strings.Cut(lines[i], " ")
		n, err := strconv.ParseInt(count, 10, 64)
		if !ok || err != nil || n < 0 || n > FileMax || kind != "literal" && kind != "delta" {
			return i, 0, refused("binary patch exceeds the per-file limit or has an invalid block")
		}
		i++
		var compressed bytes.Buffer
		for i < len(lines) && lines[i] != "" {
			line := lines[i]
			if len(line) < 6 {
				return i, 0, refused("invalid binary patch encoding")
			}
			size := int(line[0]-'A') + 1
			if line[0] >= 'a' && line[0] <= 'z' {
				size = int(line[0]-'a') + 27
			} else if line[0] < 'A' || line[0] > 'Z' {
				return i, 0, refused("invalid binary patch line length")
			}
			if len(line)-1 != ((size+3)/4)*5 {
				return i, 0, refused("invalid binary patch line size")
			}
			var decoded []byte
			for j := 1; j < len(line); j += 5 {
				v := uint64(0)
				for _, c := range line[j : j+5] {
					k := strings.IndexRune(gitBase85, c)
					if k < 0 {
						return i, 0, refused("invalid binary patch digit")
					}
					v = v*85 + uint64(k)
				}
				if v > 0xffffffff {
					return i, 0, refused("binary patch digit overflow")
				}
				decoded = binary.BigEndian.AppendUint32(decoded, uint32(v))
			}
			compressed.Write(decoded[:size])
			i++
		}
		z, err := zlib.NewReader(bytes.NewReader(compressed.Bytes()))
		if err != nil {
			return i, 0, refused("invalid binary patch compression: %v", err)
		}
		data, err := io.ReadAll(io.LimitReader(z, FileMax+1))
		closeErr := z.Close()
		if err != nil || closeErr != nil || int64(len(data)) != n {
			return i, 0, refused("binary patch expansion differs from its bounded declaration")
		}
		if kind == "delta" {
			base, read := deltaSize(data)
			target, _ := deltaSize(data[read:])
			if base < 0 || target < 0 || base > FileMax || target > FileMax {
				return i, 0, refused("binary delta expands beyond the per-file limit")
			}
			n = target
		}
		if block == 0 {
			expanded = n
		}
		i++ // empty separator
	}
	return i - 1, expanded, nil
}

func deltaSize(data []byte) (int64, int) {
	n := uint64(0)
	for i, c := range data {
		if i >= 9 {
			return -1, i
		}
		n |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return int64(n), i + 1
		}
	}
	return -1, len(data)
}
