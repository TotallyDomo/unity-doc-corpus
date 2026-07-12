package main

// Shared-content (corpus-common) loss detection for the audit - M0042-S6, spec amendment
// "shared-content (corpus-common) loss detection". See audit.go's package comment and
// docs/DESIGN.md for the failure class this closes.
//
// The page-local shingle invariant (audit.go) ignores every shingle whose reference document
// frequency is above max-shingle-df, treating it as chrome. That conflates page chrome with
// SHARED CONTENT (sentences repeated across many pages, e.g. the hideFlags description on 327
// pages). A transform regression stripping a shared sentence corpus-wide passes the page-local
// check clean. Two mechanisms close it, split by what a single run can and cannot see:
//
//   - Part A (live, in auditPage): a high-ref-DF shingle is CONTENT, not chrome, when it is
//     also present in the derived Markdown broadly (md-DF >= content-min-df). A content shingle
//     missing from a page's Markdown is a miss like any page-unique one. This catches a strip
//     that leaves the content on >= 1 page (partial), with no persisted state. Measured 0
//     clean-corpus false positives (the md-DF distribution over high-ref-DF shingles is sharply
//     bimodal - 90.5% content / 9.5% chrome / 0.05% ambiguous).
//   - Part B (this file, --shared-baseline): a TOTAL strip drops a shared shingle's md-DF to 0,
//     which re-reads as chrome, so Part A goes blind. A single run cannot separate a totally
//     stripped shingle from real chrome (both are high ref-DF, md-DF 0), so it needs a recorded
//     prior: the manifest pins the content-classified shingle set from a known-good corpus. A
//     manifest shingle that is STILL shared in the source (ref-DF > max-df) but has collapsed in
//     the derived side (md-DF <= max-df) is a shared-content-loss regression and gates. Anchoring
//     on ref-DF > max-df excludes source-side churn (a page legitimately removed drops out of
//     ref-DF too), so the gate fires on transform regressions only.

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// sharedBaseline pins the CONTENT-classified shingle set of a known-good corpus. The
// fingerprints are stored delta-varint-base64 (sorted uint64 deltas) so ~70K entries stay a
// few hundred KB instead of a multi-MB key list.
type sharedBaseline struct {
	Description  string `json:"description"`
	ShingleN     int    `json:"shingle_n"`
	MaxShingleDF int    `json:"max_shingle_df"`
	ContentMinDF int    `json:"content_min_df"`
	ShingleCount int    `json:"shingle_count"`
	Fingerprints string `json:"fingerprints_delta_varint_b64"`

	set map[uint64]struct{} // decoded membership set (not serialized)
}

const sharedBaselineDescription = "Content-classified shingle fingerprints of a known-good " +
	"unity-doc-corpus (M0042-S6). audit --shared-baseline gates when a pinned shingle is still " +
	"shared in the source HTML (ref-DF > max_shingle_df) but has collapsed in the derived " +
	"Markdown (md-DF <= max_shingle_df) - a corpus-wide shared-content strip. Regenerate with " +
	"--write-shared-baseline only after a human triages the change."

// encodeFingerprints sorts the fingerprints ascending, delta-encodes, and varint+base64 packs
// them. Sorting + deltas keep the byte stream small and the file diff stable.
func encodeFingerprints(fps []uint64) string {
	sort.Slice(fps, func(i, j int) bool { return fps[i] < fps[j] })
	buf := make([]byte, 0, len(fps)*3)
	var tmp [binary.MaxVarintLen64]byte
	var prev uint64
	for _, fp := range fps {
		n := binary.PutUvarint(tmp[:], fp-prev)
		buf = append(buf, tmp[:n]...)
		prev = fp
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// decodeFingerprints reverses encodeFingerprints into a membership set.
func decodeFingerprints(s string) (map[uint64]struct{}, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("shared baseline fingerprints not valid base64: %w", err)
	}
	set := make(map[uint64]struct{})
	var acc uint64
	for i := 0; i < len(raw); {
		d, n := binary.Uvarint(raw[i:])
		if n <= 0 {
			return nil, fmt.Errorf("shared baseline fingerprints corrupt at byte %d", i)
		}
		acc += d
		set[acc] = struct{}{}
		i += n
	}
	return set, nil
}

// writeSharedBaseline captures the run's content-classified shingle set as the manifest.
func writeSharedBaseline(path string, content map[uint64]struct{}, cfg auditConfig) error {
	fps := make([]uint64, 0, len(content))
	for fp := range content {
		fps = append(fps, fp)
	}
	b := sharedBaseline{
		Description:  sharedBaselineDescription,
		ShingleN:     cfg.shingleN,
		MaxShingleDF: int(cfg.maxDF),
		ContentMinDF: cfg.contentMinDF,
		ShingleCount: len(fps),
		Fingerprints: encodeFingerprints(fps),
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func loadSharedBaseline(path string) (*sharedBaseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b sharedBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing shared baseline %s: %w", path, err)
	}
	b.set, err = decodeFingerprints(b.Fingerprints)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &b, nil
}

// configMismatch reports how the manifest's recorded shingle geometry differs from the active
// run, or "" when they agree. This matters because the pinned fingerprints are width-shingleN
// hashes classified against (max_shingle_df, content_min_df): audited under a different
// --shingle-n, every pinned fingerprint's document frequency reads as 0, so sharedCollapsed
// finds nothing and the Part B gate silently passes regardless of real loss - the exact
// silent-no-op class this master exists to prevent. The audit already refuses rather than
// certifies on a page-count mismatch or a malformed record; a config-mismatched manifest is the
// same kind of "cannot apply, so do not pretend to" and must refuse too. A recorded field of 0
// predates the field (no such manifest ships, but hand-authored ones might) and is not checked,
// degrading to the prior behavior rather than hard-failing.
func (b *sharedBaseline) configMismatch(cfg auditConfig) string {
	var diffs []string
	if b.ShingleN != 0 && b.ShingleN != cfg.shingleN {
		diffs = append(diffs, fmt.Sprintf("shingle-n manifest=%d run=%d", b.ShingleN, cfg.shingleN))
	}
	if b.MaxShingleDF != 0 && b.MaxShingleDF != int(cfg.maxDF) {
		diffs = append(diffs, fmt.Sprintf("max-shingle-df manifest=%d run=%d", b.MaxShingleDF, cfg.maxDF))
	}
	if b.ContentMinDF != 0 && b.ContentMinDF != cfg.contentMinDF {
		diffs = append(diffs, fmt.Sprintf("content-min-df manifest=%d run=%d", b.ContentMinDF, cfg.contentMinDF))
	}
	return strings.Join(diffs, ", ")
}

// buildContentShingles returns the content-classified high-ref-DF shingle set: shared in the
// source (ref-DF > max-df) and present in the derived Markdown broadly (md-DF >= content-min-df).
func buildContentShingles(refDF, mdDF *dfCounter, cfg auditConfig) map[uint64]struct{} {
	content := make(map[uint64]struct{})
	for si := range refDF.shards {
		refDF.shards[si].mu.Lock()
		for fp, r := range refDF.shards[si].m {
			if r > cfg.maxDF && mdDF.get(fp) >= uint32(cfg.contentMinDF) {
				content[fp] = struct{}{}
			}
		}
		refDF.shards[si].mu.Unlock()
	}
	return content
}

// sharedCollapsed returns the manifest shingles that regressed: still shared in the source
// (ref-DF > max-df) yet collapsed in the derived Markdown (md-DF <= max-df).
func sharedCollapsed(shared *sharedBaseline, refDF, mdDF *dfCounter, cfg auditConfig) map[uint64]struct{} {
	collapsed := make(map[uint64]struct{})
	for fp := range shared.set {
		if refDF.get(fp) > cfg.maxDF && mdDF.get(fp) <= cfg.maxDF {
			collapsed[fp] = struct{}{}
		}
	}
	return collapsed
}
