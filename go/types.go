package main

import (
	"sync"
)

type parsedPage struct {
	Title string

	Canonical string

	Headings []string

	Links []link

	Body string
}

type link struct {
	Text string

	Href string
}

type record struct {
	CanonicalURL string `json:"canonical_url"`

	PageID string `json:"page_id"`

	PageKey string `json:"page_key"`

	Section string `json:"section"`

	SourceBytes int `json:"source_bytes"`

	SourceRel string `json:"source_rel"`

	SourceSHA256 []byte `json:"-"`

	TextBytes int `json:"text_bytes"`

	TextSHA256 []byte `json:"-"`

	Title string `json:"title"`

	Body string `json:"-"`

	MD []byte `json:"-"`
}

type transformJob struct {
	Index int

	Section string

	Path string
}

type transformResult struct {
	Record record

	Err error
}

type stageTimer struct {
	mu sync.Mutex

	seconds map[string]float64
}
