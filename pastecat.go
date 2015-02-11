/* Copyright (c) 2014-2015, Daniel Martí <mvdan@mvdan.cc> */
/* See LICENSE for licensing information */

package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/mvdan/pastecat/storage"

	"github.com/mvdan/bytesize"
	"github.com/mvdan/pflag"
)

const (
	// Name of the HTTP form field when uploading a paste
	fieldName = "paste"
	// Content-Type when serving pastes
	contentType = "text/plain; charset=utf-8"
	// Report usage stats how often
	reportInterval = 1 * time.Minute

	// HTTP response strings
	invalidID     = "invalid paste id"
	unknownAction = "unsupported action"
)

var (
	siteURL   = pflag.StringP("url", "u", "http://localhost:8080", "URL of the site")
	listen    = pflag.StringP("listen", "l", ":8080", "Host and port to listen to")
	lifeTime  = pflag.DurationP("lifetime", "t", 24*time.Hour, "Lifetime of the pastes")
	maxNumber = pflag.IntP("max-number", "m", 0, "Maximum number of pastes to store at once")

	maxSize    = 1 * bytesize.MB
	maxStorage = 1 * bytesize.GB
)

func init() {
	pflag.VarP(&maxSize, "max-size", "s", "Maximum size of pastes")
	pflag.VarP(&maxStorage, "max-storage", "M", "Maximum storage size to use at once")
}

func getContentFromForm(r *http.Request) ([]byte, error) {
	if value := r.FormValue(fieldName); len(value) > 0 {
		return []byte(value), nil
	}
	if f, _, err := r.FormFile(fieldName); err == nil {
		defer f.Close()
		content, err := ioutil.ReadAll(f)
		if err == nil && len(content) > 0 {
			return content, nil
		}
	}
	return nil, errors.New("no paste provided")
}

func setHeaders(header http.Header, id storage.ID, paste storage.Paste) {
	modTime := paste.ModTime()
	header.Set("Etag", fmt.Sprintf("%d-%s", modTime.Unix(), id))
	if *lifeTime > 0 {
		deathTime := modTime.Add(*lifeTime)
		lifeLeft := deathTime.Sub(time.Now())
		header.Set("Expires", deathTime.UTC().Format(http.TimeFormat))
		header.Set("Cache-Control", fmt.Sprintf(
			"max-age=%.f, must-revalidate", lifeLeft.Seconds()))
	}
	header.Set("Content-Type", contentType)
}

func handleGet(store storage.Store, w http.ResponseWriter, r *http.Request) {
	if _, e := templates[r.URL.Path]; e {
		err := tmpl.ExecuteTemplate(w, r.URL.Path,
			struct {
				SiteURL   string
				MaxSize   bytesize.ByteSize
				LifeTime  time.Duration
				FieldName string
			}{
				SiteURL:   *siteURL,
				MaxSize:   maxSize,
				LifeTime:  *lifeTime,
				FieldName: fieldName,
			})
		if err != nil {
			log.Printf("Error executing template for %s: %s", r.URL.Path, err)
		}
		return
	}
	id, err := storage.IDFromString(r.URL.Path[1:])
	if err != nil {
		http.Error(w, invalidID, http.StatusBadRequest)
		return
	}
	paste, err := store.Get(id)
	if err == storage.ErrPasteNotFound {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("Unknown error on GET: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer paste.Close()
	setHeaders(w.Header(), id, paste)
	http.ServeContent(w, r, "", paste.ModTime(), paste)
}

func handlePost(store storage.Store, stats *storage.Stats, w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxSize))
	content, err := getContentFromForm(r)
	size := int64(len(content))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := stats.MakeSpaceFor(size); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
	id, err := store.Put(content)
	if err != nil {
		log.Printf("Unknown error on POST: %s", err)
		stats.FreeSpace(size)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	storage.SetupPasteDeletion(store, stats, id, size, *lifeTime)
	fmt.Fprintf(w, "%s/%s\n", *siteURL, id)
}

func newHandler(store storage.Store, stats *storage.Stats) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			handleGet(store, w, r)
		case "POST":
			handlePost(store, stats, w, r)
		default:
			http.Error(w, unknownAction, http.StatusBadRequest)
		}
	})
}

func setupStore(stats *storage.Stats, lifeTime time.Duration, storageType string, args []string) (storage.Store, error) {
	params, e := map[string]map[string]string{
		"fs": {
			"dir": "pastes",
		},
		"fs-mmap": {
			"dir": "pastes",
		},
		"mem": {},
	}[storageType]
	if !e {
		return nil, fmt.Errorf("unknown storage type '%s'", storageType)
	}
	if len(args) > len(params) {
		return nil, fmt.Errorf("too many arguments given for %s", storageType)
	}
	for k := range params {
		if len(args) == 0 {
			break
		}
		params[k] = args[0]
		args = args[1:]
	}
	switch storageType {
	case "fs":
		log.Printf("Starting up file store in the directory '%s'", params["dir"])
		return storage.NewFileStore(stats, lifeTime, params["dir"])
	case "fs-mmap":
		log.Printf("Starting up mmapped file store in the directory '%s'", params["dir"])
		return storage.NewMmapStore(stats, lifeTime, params["dir"])
	case "mem":
		log.Printf("Starting up in-memory store")
		return storage.NewMemStore()
	}
	return nil, nil
}

func main() {
	pflag.Parse()
	if maxStorage > 1*bytesize.EB {
		log.Fatalf("Specified a maximum storage size that would overflow int64!")
	}
	if maxSize > 1*bytesize.EB {
		log.Fatalf("Specified a maximum paste size that would overflow int64!")
	}
	loadTemplates()
	stats := storage.Stats{
		MaxNumber:  *maxNumber,
		MaxStorage: int64(maxStorage),
	}
	log.Printf("siteURL    = %s", *siteURL)
	log.Printf("listen     = %s", *listen)
	log.Printf("lifeTime   = %s", *lifeTime)
	log.Printf("maxSize    = %s", maxSize)
	log.Printf("maxNumber  = %d", *maxNumber)
	log.Printf("maxStorage = %s", maxStorage)

	args := pflag.Args()
	if len(args) == 0 {
		args = []string{"fs"}
	}
	store, err := setupStore(&stats, *lifeTime, args[0], args[1:])
	if err != nil {
		log.Fatalf("Could not setup paste store: %s", err)
	}

	statsReport := func() {
		num, stg := stats.Report()
		var numStats, stgStats string
		if stats.MaxNumber > 0 {
			numStats = fmt.Sprintf("%d (%.2f%% out of %d)", num,
				float64(num*100)/float64(stats.MaxNumber), stats.MaxNumber)
		} else {
			numStats = fmt.Sprintf("%d", num)
		}
		if stats.MaxStorage > 0 {
			stgStats = fmt.Sprintf("%s (%.2f%% out of %s)", bytesize.ByteSize(stg),
				float64(stg*100)/float64(stats.MaxStorage), bytesize.ByteSize(stats.MaxStorage))
		} else {
			stgStats = fmt.Sprintf("%s", stg)
		}
		log.Printf("Have a total of %s pastes using %s", numStats, stgStats)
	}

	ticker := time.NewTicker(reportInterval)
	go func() {
		for range ticker.C {
			statsReport()
		}
	}()
	http.HandleFunc("/", newHandler(store, &stats))
	log.Println("Up and running!")
	statsReport()
	log.Fatal(http.ListenAndServe(*listen, nil))
}
