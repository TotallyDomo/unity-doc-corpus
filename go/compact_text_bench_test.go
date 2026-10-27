package main

import (
	"strconv"
	"strings"
	"testing"
)

func BenchmarkCompactTextLines(b *testing.B) {
	for _, lines := range []int{64, 256, 1024, 4096} {
		b.Run(strconv.Itoa(lines), func(b *testing.B) {
			input := strings.Repeat(" \t"+strings.Repeat("word ", 12)+"\t \n", lines)
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if compactText(input) == "" {
					b.Fatal("nonempty lines were discarded")
				}
			}
		})
	}
}
