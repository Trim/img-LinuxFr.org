package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/bmizerany/pat"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
)

// An entry of cache
type CacheEntry struct {
	ContentType string
	Body        []byte
}

// The maximal size for an image is 5MB
const maxSize = 5 * (1 << 20)

// The directory for caching files
var directory string

// The secret for checking the HMAC on requests
var secret []byte

// Fetch image from cache
func fetchImageFromCache(filename string) (contentType string, body []byte, ok bool) {
	ok = false

	f, err := os.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()

	var entry CacheEntry
	dec := gob.NewDecoder(f)
	err = dec.Decode(&entry)
	if err != nil {
		log.Printf("Error while decoding %s\n", filename)
	} else {
		contentType = entry.ContentType
		body = entry.Body
		ok = true
	}

	return
}

// Save the body and the headers in cache (on disk)
func saveImageInCache(filename string, contentType string, body []byte) {
	dirname := path.Dir(filename)
	err := os.MkdirAll(dirname, 0755)
	if err != nil {
		return
	}

	f, err := os.OpenFile(filename+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		log.Printf("Error while opening %s\n", filename)
		return
	}
	defer f.Close()

	enc := gob.NewEncoder(f)
	err = enc.Encode(CacheEntry{contentType, body})
	if err != nil {
		log.Printf("Error while encoding %s\n", filename)
		os.Remove(filename + ".tmp")
		return
	}
	err = os.Rename(filename+".tmp", filename)
	if err != nil {
		log.Printf("Error while renaming %s\n", filename)
		os.Remove(filename + ".tmp")
	}
}

// Generate a key for cache from a string
func generateKeyForCache(s string) string {
	h := sha1.New()
	io.WriteString(h, s)
	key := h.Sum(nil)

	// Use 3 levels of hasing to avoid having too many files in the same directory
	return fmt.Sprintf("%s/%x/%x/%x/%x", directory, key[0:1], key[1:2], key[2:3], key[3:])
}

// Fetch the image from the distant server
func fetchImageFromServer(uri string) (contentType string, body []byte, err error) {
	res, err := http.Get(uri)
	if err != nil {
		return
	}
	if res.StatusCode != 200 {
		log.Printf("Status code of %s is: %d\n", uri, res.StatusCode)
		err = errors.New("Unexpected status code")
		return
	}

	defer res.Body.Close()
	body, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return
	}
	if res.ContentLength > maxSize {
		log.Printf("Exceeded max size for %s: %d\n", uri, res.ContentLength)
		err = errors.New("Exceeded max size")
		return
	}
	contentType = res.Header.Get("Content-Type")
	if contentType[0:5] != "image" {
		log.Printf("%s has an invalid content-type: %s\n", uri, contentType)
		err = errors.New("Invalid content-type")
		return
	}
	log.Printf("Fetch %s (%s)\n", uri, contentType)

	return
}

// Fetch image from cache if available, or from the server
func fetchImage(uri string) (contentType string, body []byte, err error) {
	key := generateKeyForCache(uri)

	contentType, body, ok := fetchImageFromCache(key)
	if ok {
		return
	}

	contentType, body, err = fetchImageFromServer(uri)
	if err == nil {
		go saveImageInCache(key, contentType, body)
	}

	return
}

// Decode the URL and compute the associated HMAC
func decodeUrl(encoded_url string) (uri string, actual string, err error) {
	chars, err := hex.DecodeString(encoded_url)
	if err != nil {
		return
	}

	h := hmac.New(sha1.New, secret)
	_, err = h.Write(chars)
	if err != nil {
		return
	}
	actual = fmt.Sprintf("%x", h.Sum(nil))

	uri = string(chars)
	return
}

// Fetch the image from the URL (or cache) and respond with it
func Img(w http.ResponseWriter, r *http.Request) {
	expected := r.URL.Query().Get(":hmac")
	encoded_url := r.URL.Query().Get(":encoded_url")
	uri, actual, err := decodeUrl(encoded_url)
	if err != nil {
		log.Printf("Invalid URL %s\n", encoded_url)
		http.Error(w, "Invalid parameters", 400)
		return
	}

	// Check the validity of the URL
	u, err := url.Parse(uri)
	if err != nil {
		log.Printf("Invalid URL %s\n", uri)
		http.Error(w, "Invalid parameters", 400)
		return
	}
	h := u.Host + "XXXXXXX" // Be sure to have enough chars to avoid out of bounds
	if h[0:3] == "10." || h[0:4] == "127." || h[0:7] == "169.254" || h[0:7] == "192.168" {
		log.Printf("Invalid IP %s\n", uri)
		http.Error(w, "Invalid parameters", 400)
		return
	}
	if expected != actual {
		log.Printf("Invalid HMAC expected=%s actual=%s\n", expected, actual)
		http.Error(w, "Invalid HMAC", 403)
		return
	}

	contentType, body, err := fetchImage(uri)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Add("Content-Type", contentType)
	w.Write(body)
}

// Returns 200 OK if the server is running (for monitoring)
func Status(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "OK")
}

func main() {
	// Parse the command-line
	var addr string
	var secr string
	flag.StringVar(&addr, "a", "127.0.0.1:8000", "Bind to this address:port")
	flag.StringVar(&secr, "s", "252c38cdb9f638908fab5df7263d156c759d590b1251785fa612e7874ee9bbcc32a61f8d795e7593ca31f8f47396c497b215e1abde6e947d7e25772f30115a7e", "The secret for HMAC check")
	flag.StringVar(&directory, "d", "cache", "The directory for the caching files")
	flag.Parse()
	secret = []byte(secr)

	// Routing
	m := pat.New()
	m.Get("/status", http.HandlerFunc(Status))
	m.Get("/img/:hmac/:encoded_url", http.HandlerFunc(Img))
	http.Handle("/", m)

	// Start the HTTP server
	log.Printf("Listening on http://%s/\n", addr)
	err := http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
