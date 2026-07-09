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

	ContentChars int `json:"content_chars"`

	HeadingCount int `json:"heading_count"`

	Headings []string `json:"headings"`

	LinkCount int `json:"link_count"`

	MDRel string `json:"md_rel"`

	PageID string `json:"page_id"`

	PageKey string `json:"page_key"`

	Section string `json:"section"`

	SourceBytes int `json:"source_bytes"`

	SourceRel string `json:"source_rel"`

	SourceSHA256 string `json:"source_sha256"`

	TextBytes int `json:"text_bytes"`

	TextSHA256 string `json:"text_sha256"`

	Title string `json:"title"`

	Body string `json:"-"`
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
