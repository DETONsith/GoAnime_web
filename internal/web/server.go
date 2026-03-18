package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/alvarorichard/Goanime/internal/api"
	"github.com/alvarorichard/Goanime/internal/models"
	"github.com/alvarorichard/Goanime/internal/player"
	"github.com/alvarorichard/Goanime/internal/scraper"
	"github.com/alvarorichard/Goanime/internal/util"
)

//go:embed static/*
var staticFS embed.FS

type memoryStore struct {
	mu    sync.RWMutex
	next  uint64
	media map[string]*models.Anime
}

func newMemoryStore() *memoryStore {
	return &memoryStore{media: make(map[string]*models.Anime)}
}

func (s *memoryStore) putMedia(m *models.Anime) string {
	id := strconv.FormatUint(atomic.AddUint64(&s.next, 1), 10)
	s.mu.Lock()
	s.media[id] = m
	s.mu.Unlock()
	return id
}

func (s *memoryStore) getMedia(id string) (*models.Anime, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.media[id]
	return m, ok
}

// Server provides HTTP API and UI for GoAnime sources.
type Server struct {
	mux          *http.ServeMux
	store        *memoryStore
	mediaManager *scraper.MediaManager
}

type mediaItem struct {
	ID        string `json:"id"`
	AnimeName string `json:"anime_name"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	ImageURL  string `json:"image_url"`
	MediaType string `json:"media_type"`
	Year      string `json:"year"`
	Language  string `json:"language"`
	MediaURL  string `json:"media_url"`
	IMDBID    string `json:"imdb_id"`
}

// NewServer creates a new web server instance.
func NewServer() *Server {
	s := &Server{
		mux:          http.NewServeMux(),
		store:        newMemoryStore(),
		mediaManager: scraper.NewMediaManager(),
	}
	s.routes()
	return s
}

// Start runs the HTTP server.
func (s *Server) Start(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/search", s.handleSearch)
	s.mux.HandleFunc("/api/media/resolve", s.handleMediaResolve)
	s.mux.HandleFunc("/api/media/", s.handleMediaRoutes)
	s.mux.HandleFunc("/api/proxy", s.handleProxy)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "failed to load UI", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) < 2 {
		writeErr(w, http.StatusBadRequest, "query must have at least 2 characters")
		return
	}

	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))

	results, err := s.searchMedia(query, source)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	items := make([]mediaItem, 0, len(results))
	for _, media := range results {
		items = append(items, s.toMediaItem(media))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items": items,
	})
}

func (s *Server) toMediaItem(media *models.Anime) mediaItem {
	id := s.store.putMedia(media)
	mediaType := string(media.MediaType)
	if mediaType == "" {
		mediaType = string(models.MediaTypeAnime)
	}

	return mediaItem{
		ID:        id,
		AnimeName: buildAnimeDisplayName(media.Name, media.URL),
		Name:      media.Name,
		Source:    media.Source,
		ImageURL:  media.ImageURL,
		MediaType: mediaType,
		Year:      media.Year,
		Language:  inferLanguageTag(media.Name),
		MediaURL:  media.URL,
		IMDBID:    media.IMDBID,
	}
}

func (s *Server) handleMediaResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	mediaURL := strings.TrimSpace(r.URL.Query().Get("media_url"))
	if mediaURL == "" {
		writeErr(w, http.StatusBadRequest, "media_url is required")
		return
	}

	mediaType := strings.TrimSpace(r.URL.Query().Get("media_type"))
	if mediaType == "" {
		mediaType = string(models.MediaTypeAnime)
	}

	media := &models.Anime{
		Name:      strings.TrimSpace(r.URL.Query().Get("name")),
		URL:       mediaURL,
		Source:    strings.TrimSpace(r.URL.Query().Get("source")),
		ImageURL:  strings.TrimSpace(r.URL.Query().Get("image_url")),
		MediaType: models.MediaType(mediaType),
		Year:      strings.TrimSpace(r.URL.Query().Get("year")),
		IMDBID:    strings.TrimSpace(r.URL.Query().Get("imdb_id")),
	}

	item := s.toMediaItem(media)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"item": item,
	})
}

func (s *Server) searchMedia(query, source string) ([]*models.Anime, error) {
	sm := s.mediaManager.GetScraperManager()

	switch source {
	case "allanime":
		t := scraper.AllAnimeType
		return sm.SearchAnime(query, &t)
	case "animefire":
		t := scraper.AnimefireType
		return sm.SearchAnime(query, &t)
	case "flixhq":
		fallthrough
	case "movie":
		fallthrough
	case "tv":
		media, err := s.mediaManager.SearchMoviesAndTV(query)
		if err != nil {
			return nil, err
		}
		converted := scraper.ConvertFlixHQToAnime(media)
		if source == "movie" {
			return filterMediaType(converted, models.MediaTypeMovie), nil
		}
		if source == "tv" {
			return filterMediaType(converted, models.MediaTypeTV), nil
		}
		return converted, nil
	case "anime":
		return s.mediaManager.SearchAnimeOnly(query)
	default:
		return s.mediaManager.SearchAll(query)
	}
}

func filterMediaType(in []*models.Anime, mediaType models.MediaType) []*models.Anime {
	out := make([]*models.Anime, 0, len(in))
	for _, m := range in {
		if m.MediaType == mediaType {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) handleMediaRoutes(w http.ResponseWriter, r *http.Request) {
	// Supported routes:
	// /api/media/{id}/episodes
	// /api/media/{id}/stream
	// /api/media/{id}/download
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/media/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	id := parts[0]
	action := parts[1]

	media, ok := s.store.getMedia(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "media not found")
		return
	}

	switch action {
	case "episodes":
		s.handleEpisodes(w, r, media)
	case "stream":
		s.handleStream(w, r, media)
	case "download":
		s.handleDownload(w, r, media)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleEpisodes(w http.ResponseWriter, r *http.Request, media *models.Anime) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	season := parsePositiveInt(r.URL.Query().Get("season"), 1)
	episodes, seasons, selectedSeason, err := s.getEpisodesForMedia(media, season)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	type episodeItem struct {
		Number int    `json:"number"`
		Label  string `json:"label"`
	}

	episodeItems := make([]episodeItem, 0, len(episodes))
	for _, ep := range episodes {
		num := ep.Num
		if num <= 0 {
			num = parsePositiveInt(ep.Number, 1)
		}
		label := strings.TrimSpace(ep.Number)
		if label == "" {
			label = fmt.Sprintf("Episode %d", num)
		}
		episodeItems = append(episodeItems, episodeItem{Number: num, Label: label})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"seasons":         seasons,
		"selected_season": selectedSeason,
		"episodes":        episodeItems,
	})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, media *models.Anime) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	episodeNum := parsePositiveInt(r.URL.Query().Get("episode"), 1)
	season := parsePositiveInt(r.URL.Query().Get("season"), 1)
	quality := strings.TrimSpace(r.URL.Query().Get("quality"))
	if quality == "" {
		quality = "best"
	}

	streamURL, subtitles, isEmbedOnly, err := s.resolveStream(media, season, episodeNum, quality)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	type subtitleItem struct {
		Label string `json:"label"`
		Lang  string `json:"lang"`
		URL   string `json:"url"`
	}

	subs := make([]subtitleItem, 0, len(subtitles))
	for _, sub := range subtitles {
		subs = append(subs, subtitleItem{
			Label: sub.Label,
			Lang:  sub.Language,
			URL:   "/api/proxy?target=" + url.QueryEscape(sub.URL),
		})
	}

	proxyURL := "/api/proxy?target=" + url.QueryEscape(streamURL)
	if isEmbedOnly {
		proxyURL = ""
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"stream_url": streamURL,
		"proxy_url":  proxyURL,
		"is_embed":   isEmbedOnly,
		"subtitles":  subs,
	})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, media *models.Anime) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	episodeNum := parsePositiveInt(r.URL.Query().Get("episode"), 1)
	season := parsePositiveInt(r.URL.Query().Get("season"), 1)
	quality := strings.TrimSpace(r.URL.Query().Get("quality"))
	if quality == "" {
		quality = "best"
	}

	streamURL, _, isEmbedOnly, err := s.resolveStream(media, season, episodeNum, quality)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	if isEmbedOnly {
		writeErr(w, http.StatusBadRequest, "download is not supported for embed-only sources")
		return
	}

	safeName := sanitizeFilename(fmt.Sprintf("%s_ep_%d", media.Name, episodeNum))
	proxy := "/api/proxy?target=" + url.QueryEscape(streamURL) + "&download=1&filename=" + url.QueryEscape(safeName)
	http.Redirect(w, r, proxy, http.StatusFound)
}

func (s *Server) resolveStream(media *models.Anime, season, episodeNum int, quality string) (string, []models.Subtitle, bool, error) {
	episodes, _, _, err := s.getEpisodesForMedia(media, season)
	if err != nil {
		return "", nil, false, err
	}

	ep, err := findEpisode(episodes, episodeNum)
	if err != nil {
		return "", nil, false, err
	}

	if isFlixHQMedia(media) {
		url, subtitles, err := api.GetFlixHQStreamURL(media, &ep, quality)
		return url, subtitles, false, err
	}

	// AnimeFire stream extraction in enhanced adapter is still incomplete in this codebase.
	// Use the legacy parser in non-interactive mode and honor quality from the web selector.
	if strings.Contains(strings.ToLower(media.Source), "animefire") || strings.Contains(strings.ToLower(ep.URL), "animefire") {
		episodeURL := ensureAbsoluteEpisodeURL(media.URL, ep.URL)
		url, err := player.GetVideoURLForEpisodeWithQuality(episodeURL, quality)
		return url, nil, false, err
	}

	url, err := api.GetEpisodeStreamURLEnhanced(&ep, media, quality)
	return url, nil, false, err
}

func findEpisode(episodes []models.Episode, episodeNum int) (models.Episode, error) {
	for _, ep := range episodes {
		num := ep.Num
		if num <= 0 {
			num = parsePositiveInt(ep.Number, -1)
		}
		if num == episodeNum {
			return ep, nil
		}
	}

	if episodeNum > 0 && episodeNum <= len(episodes) {
		return episodes[episodeNum-1], nil
	}

	return models.Episode{}, fmt.Errorf("episode %d not found", episodeNum)
}

func (s *Server) getEpisodesForMedia(media *models.Anime, season int) ([]models.Episode, []map[string]interface{}, int, error) {
	if isFlixHQMedia(media) {
		flix := s.mediaManager.GetFlixHQClient()
		mediaID := extractFlixHQMediaID(media.URL)
		if mediaID == "" {
			return nil, nil, 0, fmt.Errorf("failed to parse FlixHQ media ID")
		}

		if media.MediaType == models.MediaTypeMovie {
			episodes := []models.Episode{{
				Number: "1",
				Num:    1,
				URL:    mediaID,
			}}
			return episodes, nil, 0, nil
		}

		seasons, err := flix.GetSeasons(mediaID)
		if err != nil {
			return nil, nil, 0, err
		}
		if len(seasons) == 0 {
			return nil, nil, 0, fmt.Errorf("no seasons found")
		}

		if season < 1 || season > len(seasons) {
			season = 1
		}
		selected := seasons[season-1]

		flixEpisodes, err := flix.GetEpisodes(selected.ID)
		if err != nil {
			return nil, nil, 0, err
		}

		episodes := make([]models.Episode, 0, len(flixEpisodes))
		for _, ep := range flixEpisodes {
			episodes = append(episodes, models.Episode{
				Number:   strconv.Itoa(ep.Number),
				Num:      ep.Number,
				URL:      ep.DataID,
				DataID:   ep.DataID,
				SeasonID: selected.ID,
				Title: models.TitleDetails{
					English: ep.Title,
					Romaji:  ep.Title,
				},
			})
		}

		seasonItems := make([]map[string]interface{}, 0, len(seasons))
		for i, se := range seasons {
			seasonItems = append(seasonItems, map[string]interface{}{
				"number": i + 1,
				"title":  se.Title,
			})
		}

		return episodes, seasonItems, season, nil
	}

	episodes, err := api.GetAnimeEpisodesEnhanced(media)
	if err == nil {
		for i := range episodes {
			episodes[i].URL = ensureAbsoluteEpisodeURL(media.URL, episodes[i].URL)
		}
	}
	return episodes, nil, 0, err
}

func ensureAbsoluteEpisodeURL(animeURL, episodeURL string) string {
	episodeURL = strings.TrimSpace(episodeURL)
	if episodeURL == "" || strings.HasPrefix(episodeURL, "http://") || strings.HasPrefix(episodeURL, "https://") {
		return episodeURL
	}

	base, err := url.Parse(strings.TrimSpace(animeURL))
	if err != nil || base == nil || base.Scheme == "" || base.Host == "" {
		return episodeURL
	}

	next, err := base.Parse(episodeURL)
	if err != nil {
		return episodeURL
	}

	return next.String()
}

func inferLanguageTag(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "[english]"):
		return "English"
	case strings.Contains(l, "[portuguese]") || strings.Contains(l, "[português]"):
		return "Portuguese"
	case strings.Contains(l, "[movies/tv]"):
		return "Movies/TV"
	default:
		return "Unknown"
	}
}

func buildAnimeDisplayName(name, mediaURL string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed != "" {
		// Drop leading bracketed tags like [Portuguese] to keep title text clear.
		for strings.HasPrefix(trimmed, "[") {
			end := strings.Index(trimmed, "]")
			if end <= 0 {
				break
			}
			trimmed = strings.TrimSpace(trimmed[end+1:])
		}
		if trimmed != "" {
			return trimmed
		}
	}

	u, err := url.Parse(strings.TrimSpace(mediaURL))
	if err != nil {
		return "Untitled media"
	}

	base := path.Base(strings.TrimSpace(u.Path))
	base = strings.TrimSpace(strings.TrimSuffix(base, path.Ext(base)))
	if base == "" || base == "." || base == "/" {
		return "Untitled media"
	}

	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.Join(strings.Fields(base), " ")
	if base == "" {
		return "Untitled media"
	}

	return base
}

func extractFlixHQMediaID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	base := path.Base(trimmed)
	parts := strings.Split(base, "-")
	if len(parts) == 0 {
		return ""
	}
	id := parts[len(parts)-1]
	if _, err := strconv.Atoi(id); err != nil {
		return ""
	}
	return id
}

func isFlixHQMedia(media *models.Anime) bool {
	if media == nil {
		return false
	}

	source := strings.ToLower(strings.TrimSpace(media.Source))
	return source == "flixhq"
}


func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" {
		writeErr(w, http.StatusBadRequest, "missing target")
		return
	}

	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		writeErr(w, http.StatusBadRequest, "invalid target")
		return
	}

	if err := validateExternalHost(parsed.Hostname()); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to build request")
		return
	}

	if v := r.Header.Get("Range"); v != "" {
		req.Header.Set("Range", v)
	}
	if v := r.Header.Get("User-Agent"); v != "" {
		req.Header.Set("User-Agent", v)
	} else {
		req.Header.Set("User-Agent", "Mozilla/5.0 (GoAnime Web)")
	}
	if v := r.Header.Get("Referer"); v != "" {
		req.Header.Set("Referer", v)
	}

	resp, err := util.GetSharedClient().Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for _, h := range []string{"Content-Type", "Content-Range", "Accept-Ranges", "Content-Length", "ETag", "Last-Modified", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}

	if strings.EqualFold(r.URL.Query().Get("download"), "1") {
		name := sanitizeFilename(r.URL.Query().Get("filename"))
		if filepath.Ext(name) == "" {
			name += inferExtension(target, resp.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "mpegurl") || strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8") {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "failed to read playlist")
			return
		}
		rewritten := rewriteM3U8(string(body), parsed)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.WriteString(w, rewritten)
		return
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func rewriteM3U8(playlist string, base *url.URL) string {
	lines := strings.Split(playlist, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		next, err := base.Parse(trimmed)
		if err != nil {
			continue
		}
		lines[i] = "/api/proxy?target=" + url.QueryEscape(next.String())
	}
	return strings.Join(lines, "\n")
}

func validateExternalHost(host string) error {
	if host == "" {
		return errors.New("invalid host")
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host")
	}

	for _, ip := range ips {
		if api.IsDisallowedIP(ip.String()) {
			return errors.New("target host is not allowed")
		}
	}

	return nil
}

func inferExtension(target, contentType string) string {
	lowerType := strings.ToLower(contentType)
	switch {
	case strings.Contains(lowerType, "mp4"):
		return ".mp4"
	case strings.Contains(lowerType, "mpegurl") || strings.HasSuffix(strings.ToLower(target), ".m3u8"):
		return ".m3u8"
	case strings.Contains(lowerType, "webm"):
		return ".webm"
	default:
		return ".bin"
	}
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "goanime-download"
	}
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	name = replacer.Replace(name)
	return name
}

func parsePositiveInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
