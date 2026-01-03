// Import your finds from the geocaching.com website.
// To retrieve your “finds” file, go to https://www.geocaching.com/pocket/default.aspx and select “My Finds” at the bottom of the page.

package geocaching

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/timelinize/timelinize/timeline"
	"go.uber.org/zap"
)

func init() {
	err := timeline.RegisterDataSource(timeline.DataSource{
		Name:            "geocaching",
		Title:           "Geocaching GPX",
		Icon:            "geocaching.png",
		Description:     "Groundspeak My Finds Pocket Query (.gpx)",
		NewOptions:      func() any { return new(Options) },
		NewFileImporter: func() timeline.FileImporter { return new(FileImporter) },
	})
	if err != nil {
		timeline.Log.Fatal("registering data source", zap.Error(err))
	}
}

// Options configures the Geocaching data source.
type Options struct {
	// The ID of the owner entity. REQUIRED for linking entity in DB.
	OwnerEntityID uint64 `json:"owner_entity_id"`
}

// FileImporter implements the timeline.FileImporter interface.
type FileImporter struct{}

// Recognize returns whether the file is a Groundspeak GPX export.
func (FileImporter) Recognize(ctx context.Context, dirEntry timeline.DirEntry, _ timeline.RecognizeParams) (timeline.Recognition, error) {
	rec := timeline.Recognition{DirThreshold: 0.9}

	if dirEntry.IsDir() {
		return rec, nil
	}

	if strings.ToLower(path.Ext(dirEntry.Name())) != ".gpx" {
		return rec, nil
	}

	// Peek a small portion of the file to see if it declares the Groundspeak namespace or tag.
	f, err := dirEntry.FS.Open(dirEntry.Filename)
	if err != nil {
		return rec, err
	}
	defer f.Close()

	buf := make([]byte, 2048)
	n, _ := io.ReadFull(f, buf)
	snippet := strings.ToLower(string(buf[:n]))
	if strings.Contains(snippet, `xmlns:groundspeak="http://www.groundspeak.com/cache/`) ||
		strings.Contains(snippet, "<groundspeak:cache") {
		// Outrank the generic GPX importer (which also returns 1 for .gpx files)
		rec.Confidence = 0.9 // Don't touch !!!!
	}

	return rec, nil
}

// FileImport imports data from a Groundspeak GPX file or folder of files.
func (fi *FileImporter) FileImport(ctx context.Context, dirEntry timeline.DirEntry, params timeline.ImportParams) error {
	dsOpt := params.DataSourceOptions.(*Options)

	owner := timeline.Entity{ID: dsOpt.OwnerEntityID}

	return fs.WalkDir(dirEntry.FS, dirEntry.Filename, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		if strings.ToLower(path.Ext(d.Name())) != ".gpx" {
			return nil
		}

		file, err := dirEntry.FS.Open(fpath)
		if err != nil {
			return err
		}
		defer file.Close()

		gpxDoc, err := decodeGPX(file)
		if err != nil {
			return fmt.Errorf("decode GPX %s: %w", fpath, err)
		}

		for _, w := range gpxDoc.Waypoints {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			if err := params.Continue(); err != nil {
				return err
			}

			graph := waypointToGraph(w, owner, fpath)
			if graph != nil {
				select {
				case params.Pipeline <- graph:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}

		return nil
	})
}

type gpxFile struct {
	XMLName   xml.Name `xml:"gpx"`
	Name      string   `xml:"name"`
	Desc      string   `xml:"desc"`
	Author    string   `xml:"author"`
	Email     string   `xml:"email"`
	Time      string   `xml:"time"`
	Keywords  string   `xml:"keywords"`
	Waypoints []wpt    `xml:"wpt"`
}

type wpt struct {
	Lat     float64 `xml:"lat,attr"`
	Lon     float64 `xml:"lon,attr"`
	Time    string  `xml:"time"`
	Name    string  `xml:"name"`
	Desc    string  `xml:"desc"`
	URL     string  `xml:"url"`
	URLName string  `xml:"urlname"`
	Sym     string  `xml:"sym"`
	Type    string  `xml:"type"`

	Cache groundspeakCache `xml:"cache"`
}

type groundspeakCache struct {
	ID        string `xml:"id,attr"`
	Archived  bool   `xml:"archived,attr"`
	Available bool   `xml:"available,attr"`

	Name       string   `xml:"name"`
	PlacedBy   string   `xml:"placed_by"`
	Owner      gsOwner  `xml:"owner"`
	Type       string   `xml:"type"`
	Container  string   `xml:"container"`
	Attributes []gsAttr `xml:"attributes>attribute"`
	Difficulty float64  `xml:"difficulty"`
	Terrain    float64  `xml:"terrain"`
	Country    string   `xml:"country"`
	State      string   `xml:"state"`
	ShortDesc  gsText   `xml:"short_description"`
	LongDesc   gsText   `xml:"long_description"`
	Hints      string   `xml:"encoded_hints"`
	Logs       []gsLog  `xml:"logs>log"`
}

type gsOwner struct {
	ID   string `xml:"id,attr"`
	Name string `xml:",chardata"`
}

type gsAttr struct {
	ID    string `xml:"id,attr"`
	Inc   string `xml:"inc,attr"`
	Label string `xml:",chardata"`
}

type gsText struct {
	HTML bool   `xml:"html,attr"`
	Text string `xml:",chardata"`
}

type gsLog struct {
	ID     string    `xml:"id,attr"`
	Date   string    `xml:"date"`
	Type   string    `xml:"type"`
	Finder gsOwner   `xml:"finder"`
	Text   gsLogText `xml:"text"`
}

type gsLogText struct {
	Encoded bool   `xml:"encoded,attr"`
	Body    string `xml:",chardata"`
}

func decodeGPX(r io.Reader) (*gpxFile, error) {
	dec := xml.NewDecoder(r)
	var g gpxFile
	if err := dec.Decode(&g); err != nil {
		return nil, err
	}
	return &g, nil
}

func waypointToGraph(w wpt, owner timeline.Entity, fpath string) *timeline.Graph {
	// Choose timestamp:
	// - My Finds GPX usually contains only the user's own "Found it" log.
	// - If exactly one log is present, use it as the event timestamp.
	// - If multiple logs, take the newest.
	// - If no logs or parse failure, fallback to waypoint time.
	placedDay, placedDayStr := parseGPXDay(w.Time)
	ts := placedDay
	switch len(w.Cache.Logs) {
	case 1:
		if lt := parseTime(w.Cache.Logs[0].Date); !lt.IsZero() {
			ts = lt
		}
	case 0:
		// keep waypoint day
	default:
		newest := ts
		for _, l := range w.Cache.Logs {
			if lt := parseTime(l.Date); !lt.IsZero() && lt.After(newest) {
				newest = lt
			}
		}
		ts = newest
	}

	location := timeline.Location{
		Latitude:  &w.Lat,
		Longitude: &w.Lon,
	}

	meta := timeline.Metadata{
		"Geocache code": w.Name,
	}
	if w.URL != "" {
		meta["URL"] = w.URL
	}
	if w.URLName != "" {
		meta["URL name"] = w.URLName
	}
	if w.Sym != "" {
		meta["Symbol"] = w.Sym
	}
	if w.Type != "" {
		meta["Type"] = w.Type
	}

	c := w.Cache
	if c.Name != "" {
		meta["Cache name"] = c.Name
	}
	if c.Type != "" {
		meta["Cache type"] = c.Type
	}
	if c.Container != "" {
		meta["Container"] = c.Container
	}
	if c.Difficulty != 0 {
		meta["Difficulty"] = c.Difficulty
	}
	if c.Terrain != 0 {
		meta["Terrain"] = c.Terrain
	}
	meta["Archived"] = c.Archived
	meta["Available"] = c.Available
	if c.Country != "" {
		meta["Country"] = c.Country
	}
	if c.State != "" {
		meta["State"] = c.State
	}
	if c.PlacedBy != "" {
		meta["Placed by"] = c.PlacedBy
	}
	if c.Owner.Name != "" {
		meta["Owner name"] = c.Owner.Name
	}
	if c.Owner.ID != "" {
		meta["Owner ID"] = c.Owner.ID
	}
	if c.ShortDesc.Text != "" {
		meta["Short description"] = c.ShortDesc.Text
	}
	if c.LongDesc.Text != "" {
		meta["Long description"] = c.LongDesc.Text
	}
	if len(c.Attributes) > 0 {
		attrs := make([]string, 0, len(c.Attributes))
		for _, a := range c.Attributes {
			attrs = append(attrs, a.Label)
		}
		meta["Attributes"] = attrs
	}

	var latestLog *gsLog
	switch len(c.Logs) {
	case 1:
		l := &c.Logs[0]
		latestLog = l
	case 0:
		// nothing to surface
	default:
		for i := range c.Logs {
			l := &c.Logs[i]
			if latestLog == nil || parseTime(l.Date).After(parseTime(latestLog.Date)) {
				latestLog = l
			}
		}
		meta["Log count"] = len(c.Logs)
	}

	// wpt/time is treated as a day (publication/placed date), stored as YYYY-MM-DD to avoid timezone/display issues.
	if placedDayStr != "" {
		meta["Cache placed date"] = placedDayStr
	} else if strings.TrimSpace(w.Time) != "" {
		meta["Cache placed date (raw)"] = w.Time
	}

	// Choose visible content
	cacheName := c.Name
	if cacheName == "" {
		cacheName = w.URLName
	}
	if cacheName == "" {
		cacheName = w.Name
	}
	// Visible content: concise header + latest log text if available
	header := fmt.Sprintf("%s (%s) D%.1f/T%.1f", cacheName, w.Name, c.Difficulty, c.Terrain)
	contentText := header
	if latestLog != nil && latestLog.Text.Body != "" {
		contentText = header + "\n\n" + latestLog.Text.Body
	}
	// Surface a title for UI components that prefer a title field
	meta["Title"] = header

	item := &timeline.Item{
		ID:                   w.Name,
		Classification:       timeline.ClassLocation,
		Timestamp:            ts,
		Location:             location,
		Owner:                owner,
		Metadata:             meta,
		OriginalLocation:     firstNonEmpty(w.URL, fpath),
		IntermediateLocation: fpath,
		Content: timeline.ItemData{
			Data: timeline.StringData(contentText),
		},
	}

	graph := &timeline.Graph{Item: item}
	// Link cache entity both as a visit (semantic) and includes (for UI display in item-entities)
	cacheEntity := makeCacheEntity(w, c)
	graph.ToEntity(timeline.RelVisit, cacheEntity)
	graph.ToEntity(timeline.RelIncludes, cacheEntity)

	// Surface latest log in metadata for quick UI access
	if latestLog != nil && meta != nil {
		meta["Latest log by"] = latestLog.Finder.Name
		meta["Latest log type"] = latestLog.Type
		meta["Latest log text"] = latestLog.Text.Body
		meta["Latest log date"] = latestLog.Date
	}

	// Attach cache owner as an entity for browsing caches by owner
	if c.Owner.Name != "" {
		graph.ToEntity(timeline.RelIncludes, makeOwnerEntity(c.Owner))
	}

	// NOTE: For list-page “item-entities” display: the UI currently renders `includes`
	// relations in item.js (detail page) but not in items.js (list page).
	// If we ever want cache/owner to appear on the list page without touching items.js,
	// we’d need to:
	//   - either modify items.js to render rel.label == 'includes' (preferred),
	//   - or (less ideal) emit synthetic items for cache/owner and link them,
	//     which is discouraged because cache/owner are entities, not items.
	//   - or change the backend to surface an “entity” field for the cache/owner,
	//     but that would mix semantics with the owner/payload entity.
	return graph
}

func makeOwnerEntity(owner gsOwner) *timeline.Entity {
	return &timeline.Entity{
		Type: timeline.EntityPerson,
		Name: owner.Name,
		Attributes: []timeline.Attribute{
			{
				Name:     "geocaching_username",
				Value:    owner.Name,
				Identity: true,
				Metadata: timeline.Metadata{
					"Geocaching owner ID": owner.ID,
				},
			},
		},
		Metadata: timeline.Metadata{
			"Geocaching owner ID": owner.ID,
		},
	}
}

func makeCacheEntity(w wpt, c groundspeakCache) *timeline.Entity {
	name := c.Name
	if name == "" {
		name = w.URLName
	}
	if name == "" {
		name = w.Name
	}

	lat := w.Lat
	lon := w.Lon

	ent := &timeline.Entity{
		Type: timeline.EntityPlace,
		Name: name,
		Attributes: []timeline.Attribute{
			{
				Name:     "geocache_code",
				Value:    w.Name,
				Identity: true,
			},
			{
				Name:      "coordinate",
				Latitude:  &lat,
				Longitude: &lon,
				Metadata: timeline.Metadata{
					"Geocache code": w.Name,
					"URL":           w.URL,
				},
			},
		},
	}

	if c.Type != "" {
		ent.Attributes = append(ent.Attributes, timeline.Attribute{
			Name:  "geocache_type",
			Value: c.Type,
		})
	}
	if c.Container != "" {
		ent.Attributes = append(ent.Attributes, timeline.Attribute{
			Name:  "container",
			Value: c.Container,
		})
	}
	if c.Difficulty != 0 {
		ent.Attributes = append(ent.Attributes, timeline.Attribute{
			Name:  "difficulty",
			Value: c.Difficulty,
		})
	}
	if c.Terrain != 0 {
		ent.Attributes = append(ent.Attributes, timeline.Attribute{
			Name:  "terrain",
			Value: c.Terrain,
		})
	}
	return ent
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseTime(val string) time.Time {
	val = strings.TrimSpace(val)
	if val == "" {
		return time.Time{}
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, val); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseGPXDay parses a GPX day value and returns both a normalized time.Time at
// midnight UTC and a stable YYYY-MM-DD string. Falls back to zero values on failure.
func parseGPXDay(val string) (time.Time, string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return time.Time{}, ""
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, val); err == nil {
			y, m, d := t.Date()
			day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
			return day, fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)
		}
	}
	return time.Time{}, ""
}
