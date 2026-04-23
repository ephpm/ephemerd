//go:build ignore

package main

import (
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	f, _ := os.Open(`C:\ProgramData\ephemerd\vm\linux\initrd`)
	gr, _ := gzip.NewReader(f)
	var data []byte
	buf := make([]byte, 65536)
	for {
		n, err := gr.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil { break }
	}
	_ = binary.LittleEndian // unused but imported

	offset := 0
	for offset < len(data)-110 {
		magic := string(data[offset:offset+6])
		if magic != "070701" {
			offset++
			continue
		}
		
		// Parse newc header fields (all 8-char hex)
		parseHex := func(off, size int) uint64 {
			s := string(data[offset+off:offset+off+size])
			v, _ := strconv.ParseUint(s, 16, 64)
			return v
		}
		
		mode := parseHex(6, 8)
		nameSize := parseHex(94, 8)
		fileSize := parseHex(54, 8)
		
		// Name starts at offset 110
		name := string(data[offset+110:offset+110+int(nameSize)-1]) // -1 for null
		
		if name == "TRAILER!!!" { break }
		
		// Only show interesting entries
		if strings.Contains(name, "init") || strings.Contains(name, "busybox") || 
		   strings.Contains(name, "bin/sh") || strings.HasPrefix(name, "bin/") ||
		   strings.HasPrefix(name, "sbin/") {
			modeStr := fmt.Sprintf("%06o", mode)
			fmt.Printf("%-35s mode=%s size=%d\n", name, modeStr, fileSize)
		}
		
		// Advance: header(110) + name(padded to 4) + file(padded to 4)
		nameTotal := 110 + int(nameSize)
		nameTotal = (nameTotal + 3) &^ 3
		fileTotal := int(fileSize)
		fileTotal = (fileTotal + 3) &^ 3
		offset = nameTotal + fileTotal
	}
}
