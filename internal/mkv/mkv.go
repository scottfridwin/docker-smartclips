// Package mkv builds clip layouts that reuse a Matroska file's clusters byte-for-byte.
//
// A clip is a new EBML header, Segment, SeekHead, Info, Tracks and Cues followed by the
// source clusters that cover the requested range. Each cluster gets a new header with a
// shifted timestamp; its blocks are referenced unchanged, because block timestamps are
// relative to their cluster. Only the first and last clusters are filtered block by block.
package mkv

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/scottfridwin/docker-smartclips/internal/ebml"
	"github.com/scottfridwin/docker-smartclips/internal/layout"
)

// ErrUnsupported means the file is valid but can't be clipped by reference (use the remux path).
var ErrUnsupported = errors.New("mkv: unsupported layout")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

// maxElement bounds the top-level metadata elements that are read into memory.
const maxElement = 64 << 20

// CuePoint is a video keyframe position from the source Cues.
type CuePoint struct {
	Time       uint64 // timestamp ticks
	ClusterOff int64  // absolute offset of the cluster element
}

// Source is the parsed metadata of a Matroska file.
type Source struct {
	Size           int64
	EBMLHeader     []byte // raw EBML header element
	SegmentData    int64  // absolute offset of the Segment payload
	SegmentEnd     int64
	TimestampScale uint64 // nanoseconds per tick
	Duration       float64
	InfoChildren   []ebml.Element
	TracksChildren []ebml.Element
	VideoTrack     uint64
	Attachments    *[2]int64 // offset and length of the raw Attachments element
	FirstCluster   int64
	Cues           []CuePoint // video track only, sorted by time
}

func readAt(r io.ReaderAt, off, n int64) ([]byte, error) {
	if n < 0 || n > maxElement {
		return nil, unsupported("element of %d bytes at %d", n, off)
	}
	buf := make([]byte, n)
	got, err := r.ReadAt(buf, off)
	if int64(got) < n {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// readElement returns the payload of the element at off, which must have the given ID.
func readElement(r io.ReaderAt, off int64, id uint32) ([]byte, ebml.Header, error) {
	h, err := ebml.ReadHeaderAt(r, off)
	if err != nil {
		return nil, h, err
	}
	if h.ID != id {
		return nil, h, fmt.Errorf("mkv: expected element 0x%X at %d, found 0x%X", id, off, h.ID)
	}
	if h.Size == ebml.UnknownSize {
		return nil, h, unsupported("unknown-size element 0x%X", id)
	}
	b, err := readAt(r, off+int64(h.HeaderLen), h.Size)
	return b, h, err
}

// Open parses the metadata needed to cut clips from a Matroska file.
func Open(r io.ReaderAt, size int64) (*Source, error) {
	s := &Source{Size: size, TimestampScale: 1000000}
	h, err := ebml.ReadHeaderAt(r, 0)
	if err != nil {
		return nil, fmt.Errorf("mkv: %w", err)
	}
	if h.ID != ebml.IDEBML || h.Size == ebml.UnknownSize {
		return nil, unsupported("not an EBML file")
	}
	if s.EBMLHeader, err = readAt(r, 0, int64(h.HeaderLen)+h.Size); err != nil {
		return nil, err
	}
	segOff := int64(len(s.EBMLHeader))
	seg, err := ebml.ReadHeaderAt(r, segOff)
	if err != nil {
		return nil, err
	}
	if seg.ID != ebml.IDSegment {
		return nil, unsupported("no Segment after the EBML header")
	}
	s.SegmentData = segOff + int64(seg.HeaderLen)
	s.SegmentEnd = size
	if seg.Size != ebml.UnknownSize && s.SegmentData+seg.Size < size {
		s.SegmentEnd = s.SegmentData + seg.Size
	}

	// Walk top-level elements up to the first cluster; follow the SeekHead for Cues.
	var infoOff, tracksOff, cuesOff int64 = -1, -1, -1
	for off := s.SegmentData; off < s.SegmentEnd; {
		eh, err := ebml.ReadHeaderAt(r, off)
		if err != nil {
			return nil, err
		}
		if eh.ID == ebml.IDCluster {
			s.FirstCluster = off
			break
		}
		if eh.Size == ebml.UnknownSize {
			return nil, unsupported("unknown-size top-level element 0x%X", eh.ID)
		}
		switch eh.ID {
		case ebml.IDInfo:
			infoOff = off
		case ebml.IDTracks:
			tracksOff = off
		case ebml.IDCues:
			cuesOff = off
		case ebml.IDAttachments:
			s.Attachments = &[2]int64{off, int64(eh.HeaderLen) + eh.Size}
		case ebml.IDSeekHead:
			b, err := readAt(r, off+int64(eh.HeaderLen), eh.Size)
			if err != nil {
				return nil, err
			}
			if pos, ok := seekPosition(b, ebml.IDCues); ok && cuesOff < 0 {
				cuesOff = s.SegmentData + pos
			}
		}
		off += int64(eh.HeaderLen) + eh.Size
	}
	if s.FirstCluster == 0 {
		return nil, unsupported("no clusters")
	}
	if infoOff < 0 || tracksOff < 0 {
		return nil, unsupported("missing Info or Tracks")
	}
	if cuesOff < 0 {
		return nil, unsupported("no Cues index")
	}
	if err := s.parseInfo(r, infoOff); err != nil {
		return nil, err
	}
	if err := s.parseTracks(r, tracksOff); err != nil {
		return nil, err
	}
	if err := s.parseCues(r, cuesOff); err != nil {
		return nil, err
	}
	return s, nil
}

func seekPosition(seekHead []byte, want uint32) (int64, bool) {
	seeks, err := ebml.Children(seekHead)
	if err != nil {
		return 0, false
	}
	for _, seek := range seeks {
		if seek.ID != ebml.IDSeek {
			continue
		}
		kids, err := ebml.Children(seek.Data)
		if err != nil {
			continue
		}
		var id uint32
		var pos uint64
		found := false
		for _, k := range kids {
			switch k.ID {
			case ebml.IDSeekID:
				v, _ := ebml.Uint(k.Data)
				id = uint32(v)
			case ebml.IDSeekPosition:
				pos, _ = ebml.Uint(k.Data)
				found = true
			}
		}
		if id == want && found && pos < math.MaxInt64/2 {
			return int64(pos), true
		}
	}
	return 0, false
}

func (s *Source) parseInfo(r io.ReaderAt, off int64) error {
	b, _, err := readElement(r, off, ebml.IDInfo)
	if err != nil {
		return err
	}
	if s.InfoChildren, err = ebml.Children(b); err != nil {
		return err
	}
	for _, c := range s.InfoChildren {
		switch c.ID {
		case ebml.IDTimestampScale:
			if v, _ := ebml.Uint(c.Data); v > 0 {
				s.TimestampScale = v
			}
		case ebml.IDDuration:
			s.Duration, _ = ebml.Float(c.Data)
		}
	}
	return nil
}

func (s *Source) parseTracks(r io.ReaderAt, off int64) error {
	b, _, err := readElement(r, off, ebml.IDTracks)
	if err != nil {
		return err
	}
	if s.TracksChildren, err = ebml.Children(b); err != nil {
		return err
	}
	for _, t := range s.TracksChildren {
		if t.ID != ebml.IDTrackEntry {
			continue
		}
		kids, err := ebml.Children(t.Data)
		if err != nil {
			return err
		}
		var num, typ uint64
		for _, k := range kids {
			switch k.ID {
			case ebml.IDTrackNumber:
				num, _ = ebml.Uint(k.Data)
			case ebml.IDTrackType:
				typ, _ = ebml.Uint(k.Data)
			}
		}
		if typ == 1 && s.VideoTrack == 0 {
			s.VideoTrack = num
		}
	}
	if s.VideoTrack == 0 {
		return unsupported("no video track")
	}
	return nil
}

func (s *Source) parseCues(r io.ReaderAt, off int64) error {
	b, _, err := readElement(r, off, ebml.IDCues)
	if err != nil {
		return err
	}
	points, err := ebml.Children(b)
	if err != nil {
		return err
	}
	for _, p := range points {
		if p.ID != ebml.IDCuePoint {
			continue
		}
		kids, err := ebml.Children(p.Data)
		if err != nil {
			return err
		}
		var t uint64
		for _, k := range kids {
			if k.ID == ebml.IDCueTime {
				t, _ = ebml.Uint(k.Data)
			}
		}
		for _, k := range kids {
			if k.ID != ebml.IDCueTrackPositions {
				continue
			}
			pos, err := ebml.Children(k.Data)
			if err != nil {
				return err
			}
			var track, cluster uint64
			hasCluster := false
			for _, q := range pos {
				switch q.ID {
				case ebml.IDCueTrack:
					track, _ = ebml.Uint(q.Data)
				case ebml.IDCueClusterPosition:
					cluster, _ = ebml.Uint(q.Data)
					hasCluster = true
				}
			}
			if track == s.VideoTrack && hasCluster && cluster < uint64(s.Size) {
				s.Cues = append(s.Cues, CuePoint{Time: t, ClusterOff: s.SegmentData + int64(cluster)})
			}
		}
	}
	if len(s.Cues) == 0 {
		return unsupported("no video cue points")
	}
	sort.Slice(s.Cues, func(i, j int) bool { return s.Cues[i].Time < s.Cues[j].Time })
	return nil
}

// Seconds converts ticks to seconds.
func (s *Source) Seconds(ticks uint64) float64 {
	return float64(ticks) * float64(s.TimestampScale) / 1e9
}

// Ticks converts seconds to ticks.
func (s *Source) Ticks(sec float64) uint64 {
	if sec <= 0 {
		return 0
	}
	return uint64(math.Round(sec * 1e9 / float64(s.TimestampScale)))
}

// DurationSeconds is the file's duration from its Info element.
func (s *Source) DurationSeconds() float64 {
	return s.Duration * float64(s.TimestampScale) / 1e9
}

// Keyframes returns the video keyframe times (seconds) from the Cues.
func (s *Source) Keyframes() []float64 {
	out := make([]float64, len(s.Cues))
	for i, c := range s.Cues {
		out[i] = s.Seconds(c.Time)
	}
	return out
}

// cluster is one source cluster.
type cluster struct {
	off       int64 // element start
	end       int64 // element end
	blocksOff int64 // first block child
	timestamp uint64
	newOffset int64 // position relative to the clip's Segment payload
	newHeader []byte
	body      []layout.Part
}

func readCluster(r io.ReaderAt, off int64) (*cluster, error) {
	h, err := ebml.ReadHeaderAt(r, off)
	if err != nil {
		return nil, err
	}
	if h.ID != ebml.IDCluster {
		return nil, nil
	}
	if h.Size == ebml.UnknownSize {
		return nil, unsupported("unknown-size cluster at %d", off)
	}
	c := &cluster{off: off, end: off + int64(h.HeaderLen) + h.Size}
	gotTime := false
	// Children before the first block (CRC-32, Timestamp, Position, PrevSize) are dropped.
	for p := off + int64(h.HeaderLen); p < c.end; {
		ch, err := ebml.ReadHeaderAt(r, p)
		if err != nil {
			return nil, err
		}
		if ch.Size == ebml.UnknownSize {
			return nil, unsupported("unknown-size cluster child at %d", p)
		}
		switch ch.ID {
		case ebml.IDTimestamp:
			b, err := readAt(r, p+int64(ch.HeaderLen), ch.Size)
			if err != nil {
				return nil, err
			}
			c.timestamp, _ = ebml.Uint(b)
			gotTime = true
		case ebml.IDSimpleBlock, ebml.IDBlockGroup:
			c.blocksOff = p
			if !gotTime {
				return nil, unsupported("cluster at %d has blocks before its timestamp", off)
			}
			return c, nil
		case ebml.IDEncryptedBlock:
			return nil, unsupported("encrypted blocks")
		}
		p += int64(ch.HeaderLen) + ch.Size
	}
	c.blocksOff = c.end // empty cluster
	return c, nil
}

// block is one SimpleBlock or BlockGroup element.
type block struct {
	off, len int64
	track    uint64
	time     int64  // absolute ticks
	tsOff    int64  // offset of the 2-byte relative timestamp within the element
	prefix   []byte // element bytes before the timestamp
}

func readBlocks(r io.ReaderAt, c *cluster) ([]block, error) {
	body, err := readAt(r, c.blocksOff, c.end-c.blocksOff)
	if err != nil {
		return nil, err
	}
	kids, err := ebml.Children(body)
	if err != nil {
		return nil, err
	}
	var out []block
	var pos int64
	for _, k := range kids {
		raw := int64(len(k.Raw))
		data := k.Data
		switch k.ID {
		case ebml.IDSimpleBlock:
		case ebml.IDBlockGroup:
			data = nil
			gkids, err := ebml.Children(k.Data)
			if err != nil {
				return nil, err
			}
			for _, g := range gkids {
				if g.ID == ebml.IDBlock {
					data = g.Data
				}
			}
			if data == nil {
				return nil, unsupported("BlockGroup without Block")
			}
		case ebml.IDEncryptedBlock:
			return nil, unsupported("encrypted blocks")
		default:
			pos += raw
			continue
		}
		track, tl, err := ebml.ReadSize(data)
		if err != nil || len(data) < tl+3 {
			return nil, fmt.Errorf("mkv: malformed block at %d", c.blocksOff+pos)
		}
		rel := int16(uint16(data[tl])<<8 | uint16(data[tl+1]))
		// data aliases body, so its offset within the element locates the timestamp.
		tsOff := int64(cap(k.Raw)-cap(data)) + int64(tl)
		out = append(out, block{
			off: c.blocksOff + pos, len: raw, track: uint64(track),
			time: int64(c.timestamp) + int64(rel), tsOff: tsOff, prefix: k.Raw[:tsOff],
		})
		pos += raw
	}
	return out, nil
}

// Clip is the result of BuildClip.
type Clip struct {
	Parts       []layout.Part
	ActualStart float64 // seconds in the source timeline
	ActualEnd   float64
}

// BuildClip lays out a clip of [start, end) seconds titled title.
// The clip starts at the last keyframe at or before start.
func (s *Source) BuildClip(r io.ReaderAt, start, end float64, title string) (*Clip, error) {
	if end <= start {
		return nil, fmt.Errorf("mkv: end %.3f is not after start %.3f", end, start)
	}
	startT, endT := s.Ticks(start), s.Ticks(end)
	if s.Duration > 0 {
		dur := uint64(s.Duration)
		if startT >= dur {
			return nil, fmt.Errorf("mkv: start %.3fs is at or past the end of the file (%.3fs)", start, s.DurationSeconds())
		}
		endT = min(endT, dur)
	}
	ci := sort.Search(len(s.Cues), func(i int) bool { return s.Cues[i].Time > startT }) - 1
	if ci < 0 {
		ci = 0
	}
	key := s.Cues[ci]
	if key.Time >= endT {
		return nil, fmt.Errorf("mkv: start %.3fs is at or past the end of the file", start)
	}

	clusters, err := s.collectClusters(r, key.ClusterOff, endT)
	if err != nil {
		return nil, err
	}

	// Filter the first and last clusters by block; all others are referenced whole.
	first, last := clusters[0], clusters[len(clusters)-1]
	firstBlocks, err := readBlocks(r, first)
	if err != nil {
		return nil, err
	}
	keep := s.keepFrom(firstBlocks, int64(key.Time))
	if first == last {
		keep = s.keepUntil(keep, int64(endT))
	}
	// The clip timeline starts at the keyframe. When the first cluster starts earlier, it is
	// re-based onto the keyframe by rewriting just the 2-byte relative timestamp of each
	// kept block; the frame data itself is still referenced from the source.
	origin := int64(key.Time)
	if first.timestamp == key.Time {
		first.body = blockParts(keep)
	} else {
		if first.body, err = rebasedParts(keep, origin); err != nil {
			return nil, err
		}
		first.timestamp = key.Time
	}
	if first != last {
		lastBlocks, err := readBlocks(r, last)
		if err != nil {
			return nil, err
		}
		last.body = blockParts(s.keepUntil(lastBlocks, int64(endT)))
	}
	if len(clusters) > 2 {
		for _, c := range clusters[1 : len(clusters)-1] {
			c.body = []layout.Part{layout.Range(c.blocksOff, c.end-c.blocksOff)}
		}
	}
	for _, c := range clusters {
		if int64(c.timestamp) < origin {
			return nil, unsupported("cluster timestamps decrease at %d", c.off)
		}
	}

	parts := s.assemble(clusters, origin, endT, title)
	return &Clip{Parts: parts, ActualStart: s.Seconds(uint64(origin)), ActualEnd: s.Seconds(endT)}, nil
}

// collectClusters walks clusters from off until the one that contains endT.
func (s *Source) collectClusters(r io.ReaderAt, off int64, endT uint64) ([]*cluster, error) {
	var out []*cluster
	for off < s.SegmentEnd {
		c, err := readCluster(r, off)
		if err != nil {
			return nil, err
		}
		if c == nil {
			// Not a cluster (e.g. Cues or Tags after the clusters, or Void): skip it.
			h, err := ebml.ReadHeaderAt(r, off)
			if err != nil || h.Size == ebml.UnknownSize {
				break
			}
			off += int64(h.HeaderLen) + h.Size
			continue
		}
		if len(out) > 0 && c.timestamp >= endT {
			break
		}
		out = append(out, c)
		off = c.end
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mkv: no cluster at the keyframe position")
	}
	return out, nil
}

// keepFrom drops the blocks stored before the video keyframe at keyTime, and blocks that
// display before it: open-GOP leading frames reference the previous GOP and can't be decoded.
func (s *Source) keepFrom(blocks []block, keyTime int64) []block {
	start := 0
	for i, b := range blocks {
		if b.track == s.VideoTrack && b.time == keyTime {
			start = i
			break
		}
	}
	var out []block
	for _, b := range blocks[start:] {
		if b.time >= keyTime {
			out = append(out, b)
		}
	}
	return out
}

// keepUntil drops video from the first frame (in decode order) at or after endT,
// and other tracks' blocks that start at or after endT.
func (s *Source) keepUntil(blocks []block, endT int64) []block {
	var out []block
	videoDone := false
	for _, b := range blocks {
		if b.track == s.VideoTrack {
			if videoDone || b.time >= endT {
				videoDone = true
				continue
			}
		} else if b.time >= endT {
			continue
		}
		out = append(out, b)
	}
	return out
}

func blockParts(blocks []block) []layout.Part {
	var parts []layout.Part
	for _, b := range blocks {
		parts = layout.Append(parts, layout.Range(b.off, b.len))
	}
	return parts
}

// rebasedParts references blocks with their relative timestamps rewritten against base.
func rebasedParts(blocks []block, base int64) ([]layout.Part, error) {
	var parts []layout.Part
	for _, b := range blocks {
		rel := b.time - base
		if rel < math.MinInt16 || rel > math.MaxInt16 {
			return nil, unsupported("block %d ticks from the keyframe", rel)
		}
		head := append(append([]byte{}, b.prefix...), byte(uint16(rel)>>8), byte(rel))
		parts = layout.Append(parts, layout.Literal(head))
		parts = layout.Append(parts, layout.Range(b.off+b.tsOff+2, b.len-b.tsOff-2))
	}
	return parts, nil
}

func partsLen(parts []layout.Part) int64 {
	var n int64
	for _, p := range parts {
		n += p.Len
	}
	return n
}

// Elements of the source Info that don't describe the clip.
var droppedInfo = map[uint32]bool{
	ebml.IDCRC32: true, ebml.IDVoid: true, ebml.IDDuration: true, ebml.IDTitle: true,
	0x73A4: true, 0x3CB923: true, 0x3EB923: true, 0x3C83AB: true, 0x3E83BB: true, // UIDs and filenames
	0x4461: true, 0x7384: true, 0x6924: true, // DateUTC, SegmentFilename, ChapterTranslate
}

func (s *Source) assemble(clusters []*cluster, origin int64, endT uint64, title string) []layout.Part {
	var info []byte
	for _, c := range s.InfoChildren {
		if !droppedInfo[c.ID] {
			info = append(info, c.Raw...)
		}
	}
	info = ebml.AppendFloat(info, ebml.IDDuration, float64(int64(endT)-origin))
	info = ebml.AppendElement(info, ebml.IDTitle, []byte(title))
	infoEl := ebml.AppendElement(nil, ebml.IDInfo, info)

	var tracks []byte
	for _, c := range s.TracksChildren {
		if c.ID != ebml.IDCRC32 && c.ID != ebml.IDVoid {
			tracks = append(tracks, c.Raw...)
		}
	}
	tracksEl := ebml.AppendElement(nil, ebml.IDTracks, tracks)

	for _, c := range clusters {
		ts := ebml.AppendUint(nil, ebml.IDTimestamp, c.timestamp-uint64(origin))
		bodyLen := partsLen(c.body)
		h := ebml.AppendID(nil, ebml.IDCluster)
		h = ebml.AppendSizeWidth(h, int64(len(ts))+bodyLen, 8)
		c.newHeader = append(h, ts...)
	}

	// Cue points for the keyframes inside the clip, pointing at the new clusters.
	byOff := make(map[int64]*cluster, len(clusters))
	for _, c := range clusters {
		byOff[c.off] = c
	}
	type cue struct {
		time uint64
		c    *cluster
	}
	var cues []cue
	for _, cp := range s.Cues {
		if c, ok := byOff[cp.ClusterOff]; ok && int64(cp.Time) >= origin && cp.Time < endT {
			cues = append(cues, cue{cp.Time - uint64(origin), c})
		}
	}
	cuesEl := func() []byte {
		var pts []byte
		for _, q := range cues {
			var pos []byte
			pos = ebml.AppendUint(pos, ebml.IDCueTrack, s.VideoTrack)
			pos = ebml.AppendUintWidth(pos, ebml.IDCueClusterPosition, uint64(q.c.newOffset), 8)
			var pt []byte
			pt = ebml.AppendUint(pt, ebml.IDCueTime, q.time)
			pt = ebml.AppendElement(pt, ebml.IDCueTrackPositions, pos)
			pts = ebml.AppendElement(pts, ebml.IDCuePoint, pt)
		}
		return ebml.AppendElement(nil, ebml.IDCues, pts)
	}
	seekHead := func(infoPos, tracksPos, cuesPos, attPos int64) []byte {
		seek := func(id uint32, pos int64) []byte {
			var b []byte
			b = ebml.AppendElement(b, ebml.IDSeekID, ebml.AppendID(nil, id))
			b = ebml.AppendUintWidth(b, ebml.IDSeekPosition, uint64(pos), 8)
			return ebml.AppendElement(nil, ebml.IDSeek, b)
		}
		b := append(seek(ebml.IDInfo, infoPos), seek(ebml.IDTracks, tracksPos)...)
		if attPos >= 0 {
			b = append(b, seek(ebml.IDAttachments, attPos)...)
		}
		b = append(b, seek(ebml.IDCues, cuesPos)...)
		return ebml.AppendElement(nil, ebml.IDSeekHead, b)
	}

	// Element sizes don't depend on the positions (all fixed-width), so lay out once.
	attPos := int64(-1)
	if s.Attachments != nil {
		attPos = 0
	}
	infoPos := int64(len(seekHead(0, 0, 0, attPos)))
	tracksPos := infoPos + int64(len(infoEl))
	cuesPos := tracksPos + int64(len(tracksEl))
	if s.Attachments != nil {
		attPos = cuesPos
		cuesPos += s.Attachments[1]
	}
	pos := cuesPos + int64(len(cuesEl()))
	for _, c := range clusters {
		c.newOffset = pos
		pos += int64(len(c.newHeader)) + partsLen(c.body)
	}
	segmentLen := pos

	head := append([]byte{}, s.EBMLHeader...)
	head = ebml.AppendID(head, ebml.IDSegment)
	head = ebml.AppendSizeWidth(head, segmentLen, 8)
	head = append(head, seekHead(infoPos, tracksPos, cuesPos, attPos)...)
	head = append(head, infoEl...)
	head = append(head, tracksEl...)

	parts := []layout.Part{layout.Literal(head)}
	if s.Attachments != nil {
		parts = layout.Append(parts, layout.Range(s.Attachments[0], s.Attachments[1]))
	}
	parts = layout.Append(parts, layout.Literal(cuesEl()))
	for _, c := range clusters {
		parts = layout.Append(parts, layout.Literal(c.newHeader))
		for _, p := range c.body {
			parts = layout.Append(parts, p)
		}
	}
	return parts
}
