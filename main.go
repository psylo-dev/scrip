package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/segmentio/encoding/json"

	"github.com/bogem/id3v2/v2"
	"github.com/valyala/fasthttp"
)

const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.3"
const api = "api-v2.soundcloud.com"
const hlscdn = "cf-hls-media.sndcdn.com"
const imgcdn = "i1.sndcdn.com"

var scriptsRegex = regexp.MustCompile(`(?m)^<script crossorigin src="(https://a-v2\.sndcdn\.com/assets/.+\.js)"></script>$`)
var clientIdRegex = regexp.MustCompile(`\("client_id=([A-Za-z0-9]{32})"\)`)
var ErrVersionNotFound = errors.New("version not found")
var ErrScriptNotFound = errors.New("script not found")
var ErrIDNotFound = errors.New("clientid not found")
var ErrKindNotCorrect = errors.New("entity of incorrect kind")
var ErrIncompatibleStream = errors.New("incompatible stream")
var ErrNoURL = errors.New("no url")

var httpc = &fasthttp.HostClient{
	Addr:                api + ":443",
	IsTLS:               true,
	DialDualStack:       true,
	MaxIdleConnDuration: 1<<63 - 1,
}

var hlshttpc = &fasthttp.HostClient{
	Addr:                hlscdn + ":443",
	IsTLS:               true,
	DialDualStack:       true,
	MaxIdleConnDuration: 1<<63 - 1,
}

var imghttpc = &fasthttp.HostClient{
	Addr:                imgcdn + ":443",
	IsTLS:               true,
	DialDualStack:       true,
	MaxIdleConnDuration: 1<<63 - 1,
}

type User struct {
	Username  string `json:"username"`
	Kind      string `json:"kind"` // should always be "user"!
	Permalink string `json:"permalink"`
	ID        int64  `json:"id"`
	Tracks    int    `json:"track_count"`
}

type Track struct {
	Artwork       string      `json:"artwork_url"`
	CreatedAt     string      `json:"created_at"`
	Genre         string      `json:"genre"`
	Permalink     string      `json:"permalink"`
	Kind          string      `json:"kind"` // should always be "track"!
	Title         string      `json:"title"`
	Media         Media       `json:"media"`
	Authorization string      `json:"track_authorization"`
	Author        User        `json:"user"`
	Policy        TrackPolicy `json:"policy"`
	ID            int64       `json:"id"`
}

type Playlist struct {
	Permalink string  `json:"permalink"`
	Tracks    []Track `json:"tracks"`
	Kind      string  `json:"kind"` // should always be "playlist"!
}

type MissingTrack struct {
	ID    int64
	Index int
}

type TrackPolicy string

const (
	PolicyBlock TrackPolicy = "BLOCK"
)

type Protocol string

const (
	ProtocolHLS         Protocol = "hls"
	ProtocolProgressive Protocol = "progressive"
)

type Format struct {
	Protocol Protocol `json:"protocol"`
	MimeType string   `json:"mime_type"`
}

type Transcoding struct {
	URL     string `json:"url"`
	Preset  string `json:"preset"`
	Format  Format `json:"format"`
	Quality string `json:"quality"`
}

type Media struct {
	Transcodings []Transcoding `json:"transcodings"`
}

type Stream struct {
	URL string `json:"url"`
}

func (m Media) SelectCompatible() *Transcoding {
	for _, t := range m.Transcodings {
		if t.Format.Protocol == ProtocolHLS && t.Format.MimeType == "audio/mpeg" {
			return &t
		}
	}

	return nil
}

func (tr Transcoding) GetStream(authorization string) (string, error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI(tr.URL + "?client_id=" + cid + "&track_authorization=" + authorization)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := doWithRetry(httpc, req, resp)
	if err != nil {
		return "", err
	}

	if resp.StatusCode() != 200 {
		return "", fmt.Errorf("getstream: got status code %d", resp.StatusCode())
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	var s Stream
	err = json.Unmarshal(data, &s)
	if err != nil {
		return "", err
	}

	if s.URL == "" {
		return "", ErrNoURL
	}

	return s.URL, nil
}

func (tr Track) DownloadImage() ([]byte, string, error) {
	tr.Artwork = strings.Replace(tr.Artwork, "-large.", "-t500x500.", 1)
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI(tr.Artwork)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := doWithRetry(imghttpc, req, resp)
	if err != nil {
		return nil, "", err
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	return data, string(resp.Header.Peek("Content-Type")), nil
}

type Paginated[T any] struct {
	Collection []T    `json:"collection"`
	Next       string `json:"next_href"`
}

func (p *Paginated[T]) Proceed(shouldUnfold bool) error {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	oldNext := p.Next
	req.SetRequestURI(p.Next + "&client_id=" + cid)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := doWithRetry(httpc, req, resp)
	if err != nil {
		return err
	}

	if resp.StatusCode() != 200 {
		return fmt.Errorf("paginated.proceed: got status code %d", resp.StatusCode())
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	err = json.Unmarshal(data, p)
	if err != nil {
		return err
	}

	if p.Next == oldNext { // prevent loops of nothingness
		p.Next = ""
	}

	if shouldUnfold && len(p.Collection) == 0 && p.Next != "" {
		return p.Proceed(true)
	}

	return nil
}

func fmtsize(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB",
		float64(b)/float64(div), "kMGTPE"[exp])
}

func downloadHLS(url string) ([]byte, error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI(url)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := doWithRetry(hlshttpc, req, resp)
	if err != nil {
		return nil, err
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	parts := [][]byte{}
	for _, s := range bytes.Split(data, []byte{'\n'}) {
		if len(s) == 0 || s[0] == '#' {
			continue
		}

		parts = append(parts, s)
	}

	result := []byte{}
	for _, part := range parts {
		req.SetRequestURIBytes(part)

		err = doWithRetry(hlshttpc, req, resp)
		if err != nil {
			return nil, err
		}

		data, err = resp.BodyUncompressed()
		if err != nil {
			data = resp.Body()
		}

		result = append(result, data...)
	}

	return result, nil
}

func makeMetadata(fd *os.File, track Track) (int64, error) {
	tag := id3v2.NewEmptyTag()

	tag.SetArtist(track.Author.Username)
	if track.Genre != "" {
		tag.SetGenre(track.Genre)
	}

	tag.SetTitle(track.Title)

	if track.Artwork != "" {
		data, mime, err := track.DownloadImage()
		if err != nil {
			return 0, err
		}

		tag.AddAttachedPicture(id3v2.PictureFrame{MimeType: mime, Picture: data, PictureType: id3v2.PTFrontCover, Encoding: id3v2.EncodingUTF8})
	}

	return tag.WriteTo(fd)
}

func getClientID() (string, error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI("https://soundcloud.com/h")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := fasthttp.Do(req, resp)
	if err != nil {
		return "", err
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	scripts := scriptsRegex.FindAllSubmatch(data, -1)
	if len(scripts) == 0 {
		return "", ErrScriptNotFound
	}

	for _, scr := range scripts {
		if len(scr) != 2 {
			continue
		}

		req.SetRequestURIBytes(scr[1])

		err = fasthttp.Do(req, resp)
		if err != nil {
			continue
		}

		data, err = resp.BodyUncompressed()
		if err != nil {
			data = resp.Body()
		}

		res := clientIdRegex.FindSubmatch(data)
		if len(res) != 2 {
			continue
		}

		return string(res[1]), nil
	}

	return "", ErrIDNotFound
}

func doWithRetry(httpc *fasthttp.HostClient, req *fasthttp.Request, resp *fasthttp.Response) (err error) {
	for i := 0; i < 10; i++ {
		err = httpc.Do(req, resp)
		if err == nil {
			return nil
		}

		if err != fasthttp.ErrTimeout &&
			err != fasthttp.ErrDialTimeout &&
			err != fasthttp.ErrTLSHandshakeTimeout &&
			err != fasthttp.ErrConnectionClosed &&
			!os.IsTimeout(err) &&
			!errors.Is(err, syscall.EPIPE) && // EPIPE is "broken pipe" error
			err.Error() != "timeout" {
			return
		}
	}

	return
}

func resolve(path string, out any) error {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI("https://" + api + "/resolve?url=https%3A%2F%2Fsoundcloud.com%2F" + url.QueryEscape(path) + "&client_id=" + cid)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := doWithRetry(httpc, req, resp)
	if err != nil {
		return err
	}

	if resp.StatusCode() != 200 {
		return fmt.Errorf("resolve: got status code %d", resp.StatusCode())
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	return json.Unmarshal(data, out)
}

func getTracks(ids string) ([]Track, error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.SetRequestURI("https://" + api + "/tracks?ids=" + ids + "&client_id=" + cid)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	err := doWithRetry(httpc, req, resp)
	if err != nil {
		return nil, err
	}

	data, err := resp.BodyUncompressed()
	if err != nil {
		data = resp.Body()
	}

	var res []Track
	err = json.Unmarshal(data, &res)
	return res, err
}

func joinMissingTracks(missing []MissingTrack) (st string) {
	for i, track := range missing {
		st += strconv.FormatInt(track.ID, 10)
		if i != len(missing)-1 {
			st += ","
		}
	}
	return
}

func getMissingTracks(missing []MissingTrack) (res []Track, next []MissingTrack, err error) {
	if len(missing) > 50 {
		next = missing[50:]
		missing = missing[:50]
	}

	res, err = getTracks(joinMissingTracks(missing))
	return
}

func _download(basePath string, t Track) error {
	tr := t.Media.SelectCompatible()
	if tr == nil {
		return ErrIncompatibleStream
	}

	stream, err := tr.GetStream(t.Authorization)
	if err != nil {
		return err
	}

	data, err := downloadHLS(stream)
	if err != nil {
		return err
	}

	fd, err := os.OpenFile(basePath+t.Permalink+".mp3", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	_n, err := makeMetadata(fd, t)
	if err != nil {
		return fmt.Errorf("failed to add metadata: %s --- wrote %s", err, fmtsize(_n))
	}

	n, err := fd.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write track: %s --- wrote %s", err, fmtsize(int64(n)))
	}

	log.Printf("Wrote %s to %s\n", fmtsize(_n+int64(n)), t.Permalink+".mp3")
	return fd.Close()
}

func download(basePath string, t Track) (err error) {
	for i := 0; i < 5; i++ {
		err = _download(basePath, t)
		if err == nil {
			return
		}
	}
	return
}

func downloadByPath(basePath string, path string) error {
	var t Track
	err := resolve(path, &t)
	if err != nil {
		return err
	}

	if t.Kind != "track" {
		return ErrKindNotCorrect
	}

	return download(basePath, t)
}

func downloadTracks(dir string, tracks []Track) error {
	log.Printf("Downloading %d tracks\n", len(tracks))
	err := os.Mkdir(dir, 0766)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	for _, track := range tracks {
		wg.Add(1)
		go func(track Track) {
			err = download(dir+"/", track)
			if err != nil {
				log.Printf("Failed to download %s: %s\n", track.Permalink, err)
			}
			wg.Done()
		}(track)
	}

	wg.Wait()

	return nil
}

func downloadPlaylist(path string) error {
	var p Playlist
	err := resolve(path, &p)
	if err != nil {
		return err
	}

	if p.Kind != "playlist" {
		return ErrKindNotCorrect
	}

	tracks := make([]Track, 0, len(p.Tracks))
	missing := []MissingTrack{}
	for i, track := range p.Tracks {
		if track.Title == "" {
			missing = append(missing, MissingTrack{ID: track.ID, Index: i})
		} else {
			tracks = append(tracks, track)
		}
	}

	if len(missing) != 0 {
		for {
			res, next, err := getMissingTracks(missing)
			if err != nil {
				return err
			}

			tracks = append(tracks, res...)

			if len(next) == 0 {
				break
			}
			missing = next
		}
	}
	if len(tracks) == 0 {
		log.Println("No tracks in playlist")
		return nil
	}

	return downloadTracks(p.Permalink, tracks)
}

func downloadUser(path string) error {
	var u User
	err := resolve(path, &u)
	if err != nil {
		return err
	}

	if u.Kind != "user" {
		return ErrKindNotCorrect
	}

	tracks := make([]Track, 0, u.Tracks)

	// 80k is max
	p := Paginated[Track]{Next: "https://" + api + "/users/" + strconv.FormatInt(u.ID, 10) + "/tracks?limit=80000&client_id" + cid}
	for p.Next != "" {
		err = p.Proceed(true)
		if err != nil {
			return err
		}

		tracks = append(tracks, p.Collection...)
	}

	if len(tracks) == 0 {
		fmt.Println("User has no tracks")
		return nil
	}

	return downloadTracks(u.Permalink, tracks)
}

var cid string

func main() {
	{
		_cid, err := getClientID()
		if err != nil {
			log.Fatalln("failed to get clientid:", err)
		}

		cid = _cid
	}

	flag.Parse()

	in := flag.Arg(0)
	if in == "" {
		log.Fatalln("You need to provide a link for downloading")
	}

	parsed, err := url.Parse(in)
	if err == nil {
		path := parsed.Path[1:]
		if path[len(path)-1] == '/' {
			path = path[:len(path)-1]
		}

		if strings.Contains(path, "/sets/") {
			err = downloadPlaylist(path)
		} else if strings.Count(path, "/") == 0 {
			err = downloadUser(path)
		} else {
			err = downloadByPath("", path)
		}

		if err != nil {
			log.Printf("failed to download %s: %s\n", parsed.Path, err)
		}
	}
}
