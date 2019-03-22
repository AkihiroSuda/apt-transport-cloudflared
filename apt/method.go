package apt

import (
	"bufio"
	"context"
	"crypto/md5"  // #nosec
	"crypto/sha1" // #nosec
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/apt-transport-cloudflared/apt/access"
)

const (
	cfdVersion string = "0.1"
)

// CloudflaredMethod holds the fields needed to run the apt method.
type CloudflaredMethod struct {
	log      *log.Logger
	logfp    *os.File
	mwriter  *MessageWriter
	mreader  *MessageReader
	datapath string
}

// HeaderEntry represents a header to be added to a request.
type HeaderEntry struct {
	Key   string
	Value string
}

func openlog(fpath string) (*os.File, error) {
	dir := filepath.Dir(fpath)
	_, err := os.Stat(dir)
	if err != nil {
		err = os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			return nil, err
		}
	}

	// TODO: Allow user to configure append/truncate behavior
	// Attempt to open it for append
	return os.OpenFile(fpath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0600)
}

// NewCloudflaredMethod creates a new CloudflaredMethod with the given fields.
func NewCloudflaredMethod(output io.Writer, input *bufio.Reader) (*CloudflaredMethod, error) {
	// Attempt to parse together the default location.
	// Note: we run as root, so this means that our HOME directory is not the
	// users home directory. That said, os.Getenv("HOME") should still return
	// the users home directory. Always prefer the value of $HOME, though fall
	// back to asking for the user value from os/user.
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		curr, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("can not get users home directory")
		}
		home = curr.HomeDir
	}

	l := log.New(ioutil.Discard, "", 0)

	logpath := path.Join(home, ".cloudflared/cfd/log.txt")
	logfp, err := openlog(logpath)
	if err == nil {
		l.SetOutput(logfp)
	}

	return &CloudflaredMethod{
		log:      l,
		logfp:    logfp,
		mwriter:  NewMessageWriter(output),
		mreader:  NewMessageReader(input),
		datapath: path.Join(home, ".cloudflared/cfd/servicetokens/"),
	}, err
}

func (cfd *CloudflaredMethod) Close() {
	if cfd.logfp != nil {
		cfd.logfp.Close()
		cfd.logfp = nil
		cfd.log.SetOutput(ioutil.Discard)
	}
}

// Run is the main entry point for the method.
//
// This function reads messages from apt indefinitely and attempts to handle
// as many of them as possible.
func (cfd *CloudflaredMethod) Run() error {
	return cfd.RunWithReader(os.Stdin)
}

// RunWithReader reads and dispatches methods from the given reader until EOF.
func (cfd *CloudflaredMethod) RunWithReader(reader io.Reader) error {
	cfd.mwriter.Capabilities(cfdVersion, CapSendConfig|CapSingleInstance)
	mreader := NewMessageReader(bufio.NewReader(reader))

	// TODO: Just in case, keep a list of URLs that need to be dispatched, but haven't
	for {
		msg, err := mreader.ReadMessage()
		if err != nil {
			if err == io.EOF || err == io.ErrClosedPipe {
				break
			}

			if !(err == io.ErrNoProgress || err == io.ErrShortBuffer) {
				return err
			}
		}

		switch msg.StatusCode {
		case 600: // Acquire URL
			cfd.HandleAcquire(msg)
		case 601: // Configuration
			err := cfd.ParseConfig(msg)
			if err != nil {
				msg := fmt.Sprintf("Unable to parse configuration: %v", err)
				cfd.log.Printf(msg)
				cfd.mwriter.GeneralFailure(msg)
				return err
			}
		default:
			cfd.log.Printf("Unknown message: %d %s\n", msg.StatusCode, msg.Description)
			cfd.mwriter.GeneralFailure("Unhandled Message")
		}
	}

	return nil
}

// BuildRequest creates a new http.Request for the given URI.
func (cfd *CloudflaredMethod) BuildRequest(client *http.Client, uri *url.URL) (*http.Request, error) {
	if uri.Scheme != "cfd+https" {
		cfd.log.Printf("Invalid URI Scheme: '%s'", uri.Scheme)
		return nil, fmt.Errorf("invalid URI Scheme: '%s'", uri.Scheme)
	}

	uri.Scheme = "https"

	// TODO: Allow configuring this
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfd.log.Printf("Getting JWT for: %v\n", uri)
	urlwriter := NewURLWriter(cfd.mwriter, "Auth URL: ")
	token, err := access.GetToken(ctx, uri, cfd.datapath, true, urlwriter)
	if err != nil {
		return nil, err
	}

	client.Transport = access.NewTransport(token, client.Transport)

	req, err := http.NewRequest("GET", uri.String(), nil)
	if err != nil {
		return nil, err
	}

	return req, nil
}

// HandleAcquire handles a '600 Acquire URI' message from apt.
//
// This attempts to get a token for the given host and make a request for the
// resource with the cf-access-token headers.
//
// TODO: Figure out what an IMS-Hit indicates, and if that applies to this method
func (cfd *CloudflaredMethod) HandleAcquire(msg *Message) {
	requestedURL := msg.Fields["URI"]
	filename := msg.Fields["Filename"]

	cfd.log.Printf("Handle Acquire:\n  URL: %s\n  Filename: %s", requestedURL, filename)

	// TODO: Handle empty URI or Filename
	// This shouldn't happen, but it's best to be absurdly fault tolerant if possible

	uri, err := url.Parse(requestedURL)
	if err != nil {
		// Have to have started the Acquire before we can fail the acquire
		cfd.mwriter.StartURI(requestedURL, "", 0, false)
		cfd.mwriter.FailedURI(requestedURL, "", fmt.Sprintf("URI Parse Failure: %v", err), false, false)
		return
	}

	err = cfd.Acquire(uri, requestedURL, filename)
	if err != nil {
		cfd.log.Printf("Error fetching %v\n%v", uri, err)
		cfd.mwriter.FailedURI(requestedURL, "", err.Error(), false, false)
	}
}

// Acquire fetches the requested resource.
func (cfd *CloudflaredMethod) Acquire(uri *url.URL, requrl, filename string) error {
	// Set up a client - we don't want to share a client, because we may want to
	// allow pipelining requests and that would mean changing the token.
	client := &http.Client{}

	// Build our request
	req, err := cfd.BuildRequest(client, uri)
	if err != nil {
		cfd.mwriter.StartURI(requrl, "", 0, false)
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		cfd.mwriter.StartURI(requrl, "", 0, false)
		return err
	}

	// Handle non-200 responses
	// TODO: Handle other 200 codes
	if resp.StatusCode != 200 {
		cfd.mwriter.StartURI(requrl, "", 0, false)
		return fmt.Errorf("GET for %s failed with %s", uri.String(), resp.Status)
	}

	cfd.mwriter.StartURI(requrl, "", resp.ContentLength, false)

	// Close the body at the end of the method
	defer resp.Body.Close()
	// We buffer up to 16kb at a time
	buffer := make([]byte, 1024*16)

	// We want to compute our different hashes, otherwise Apt will reject the package
	hashMD5 := md5.New()   // #nosec
	hashSHA1 := sha1.New() // #nosec
	hashSHA256 := sha256.New()
	hashSHA512 := sha512.New()

	// And finally, we need to write to this file
	fp, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("error opening file '%s': %v", filename, err)
	}

	mw := io.MultiWriter(hashMD5, hashSHA1, hashSHA256, hashSHA512, fp)
	if _, err := io.CopyBuffer(mw, resp.Body, buffer); err != nil {
		return fmt.Errorf("error reading response body: %v", err)
	}

	strMD5 := fmt.Sprintf("%x", hashMD5.Sum(nil))
	strSHA1 := fmt.Sprintf("%x", hashSHA1.Sum(nil))
	strSHA256 := fmt.Sprintf("%x", hashSHA256.Sum(nil))
	strSHA512 := fmt.Sprintf("%x", hashSHA512.Sum(nil))

	cfd.mwriter.FinishURI(requrl, filename, "", "", false, false,
		Field{"MD5-Hash", strMD5},
		Field{"MD5Sum-Hash", strMD5},
		Field{"SHA1-Hash", strSHA1},
		Field{"SHA256-Hash", strSHA256},
		Field{"SHA512-Hash", strSHA512},
	)

	return nil
}

// ParseConfig takes a config message from apt and sets config values from it.
func (cfd *CloudflaredMethod) ParseConfig(msg *Message) error {
	cfd.log.Println("Parsing config:")
	for k, v := range msg.Fields {
		cfd.log.Printf("    %s %s", k, v)
	}
	return nil
}
