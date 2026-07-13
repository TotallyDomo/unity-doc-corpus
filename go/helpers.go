package main

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"runtime"
	"strings"
	"time"
)

func newStageTimer() *stageTimer {

	return &stageTimer{seconds: map[string]float64{}}

}

func (t *stageTimer) measure(name string, fn func() error) error {

	start := time.Now()

	err := fn()

	t.mu.Lock()

	t.seconds[name] += time.Since(start).Seconds()

	t.mu.Unlock()

	return err

}

func (t *stageTimer) measureValue(name string, fn func() error) error {

	return t.measure(name, fn)

}

func sha256Hex(data []byte) string {

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])

}

func sha256Bytes(data []byte) []byte {

	sum := sha256.Sum256(data)

	return append([]byte(nil), sum[:]...)

}

func defaultWorkers() int {

	workers := runtime.NumCPU() / 2

	if workers < 1 {

		return 1

	}

	return workers

}

func compactText(value string) string {

	value = html.UnescapeString(value)

	value = strings.ReplaceAll(value, "\r\n", "\n")

	value = strings.ReplaceAll(value, "\r", "\n")

	var b strings.Builder

	lastSpace := false

	for _, r := range value {

		if r == ' ' || r == '\t' || r == '\f' || r == '\v' {

			if !lastSpace {

				b.WriteRune(' ')

			}

			lastSpace = true

		} else {

			b.WriteRune(r)

			lastSpace = false

		}

	}

	value = b.String()

	b.Reset()

	for i := 0; i < len(value); i++ {

		if value[i] == '\n' {

			s := b.String()

			for strings.HasSuffix(s, " ") {

				s = strings.TrimSuffix(s, " ")

			}

			b.Reset()

			b.WriteString(s)

			b.WriteByte('\n')

			for i+1 < len(value) && value[i+1] == ' ' {

				i++

			}

		} else {

			b.WriteByte(value[i])

		}

	}

	value = b.String()

	b.Reset()

	run := 0

	for _, r := range value {

		if r == '\n' {

			run++

			if run <= 2 {

				b.WriteRune(r)

			}

		} else {

			run = 0

			b.WriteRune(r)

		}

	}

	return strings.Trim(b.String(), " \n")

}
