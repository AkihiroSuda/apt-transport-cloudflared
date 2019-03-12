package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	//"net/http/httptrace"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	cfdVersion string = "0.1"
)

// CloudflaredMethod holds the fields needed to run the apt method.
type CloudflaredMethod struct {
	Context context.Context
	Log     *log.Logger
	Client  *http.Client
	mwriter *MessageWriter
	mreader *MessageReader
}

// HeaderEntry represents a header to be added to a request.
type HeaderEntry struct {
	Key   string
	Value string
}

// For testing - we need to be able to stub out the exec.CommandContext calls
// so we put the function in a variable that can be changed if tests are
// running.
var makeCommand = exec.CommandContext

// NewCloudflaredMethod creates a new CloudflaredMethod with the given fields.
func NewCloudflaredMethod(output io.Writer, input *bufio.Reader, logFilename string) (*CloudflaredMethod, error) {
	var logger *log.Logger

	// The Client we use by default is the standard default client
	client := &http.Client{}

	// TODO: Only log when needed
	logger = nil
	return &CloudflaredMethod{
		Log:     logger,
		Client:  client,
		mwriter: NewMessageWriter(output),
		mreader: NewMessageReader(input),
	}, nil
}

// Run is the main entry point for the method.
//
// This function reads messages from apt indefinately and attempts to handle
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
			cfd.ParseConfig(msg)
		default:
			cfd.mwriter.GeneralFailure("Unhandled Message")
		}
	}

	return nil
}

// GetToken uses cloudflared to acquire a token for the requested URI.
//
// TODO: 'native' handling of token acquisition.
func (cfd *CloudflaredMethod) GetToken(ctx context.Context, uri *url.URL) ([]HeaderEntry, error) {
	// TODO: Support service tokens
	// Steps:
	//   1. Get the service token directory from the configuration message from Apt (default: ~/.cfd/servicetoken/)
	//   2. Check if the host name given is present in the service token directory
	//   3. Read the file and use that instead of using cloudflared
	// For now though, just login with cloudflared
	path := uri.Scheme + "://" + uri.Host

	login := makeCommand(ctx, "cloudflared", "access", "login", path)
	//login := exec.CommandContext(ctx, "cloudflared", "access", "login", path)
	// TODO: Display the URL that cloudflared outputs
	err := login.Run()
	if err != nil {
		return nil, err
	}

	cmd := makeCommand(ctx, "cloudflared", "access", "token", "--app", path)
	//cmd := exec.CommandContext(ctx, "cloudflared", "access", "token", "--app", path)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	// cloudflared doesn't signal an error in its exit code; we have to check
	// the output.
	// Result of "" shouldn't happen (except in testing)
	// If the 'token' starts with "Unable", then `cloudflared` failed to fetch
	// the token.
	token := strings.TrimSpace(string(output))
	if token == "" {
		return nil, errors.New("No output from `cloudflared access token`")
	}
	if strings.HasPrefix(token, "Unable") {
		return nil, errors.New("Bad output from `cloudflared access token`: unable to get token")
	}

	return []HeaderEntry{{"cf-access-token", token}}, nil
}

// BuildRequest creates a new http.Request for the given URI.
func (cfd *CloudflaredMethod) BuildRequest(uri *url.URL) (*http.Request, error) {
	switch uri.Scheme {
	case "cfd+https":
		uri.Scheme = "https"
	case "cfd":
		uri.Scheme = "https"
		cfd.mwriter.Warning("URI Scheme 'cfd' should not be used. Defaulting to cfd+https")
	default:
		return nil, fmt.Errorf("Invalid URI Scheme: '%s'", uri.Scheme)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	headers, err := cfd.GetToken(ctx, uri)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("GET", uri.String(), nil)
	if err != nil {
		return nil, err
	}

	for _, h := range headers {
		req.Header.Set(h.Key, h.Value)
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
		cfd.mwriter.FailedURI(requestedURL, "", err.Error(), false, false)
	}
}

// ParseContentLengthHeader finds a Content-Length header and converts it to a uint64.
func ParseContentLengthHeader(headers map[string][]string) uint64 {
	sizeHeader, ok := headers["Content-Length"]
	if !ok {
		return 0
	}

	size, err := strconv.ParseUint(sizeHeader[0], 10, 64)
	if err != nil {
		return 0
	}

	return size
}

// Acquire fetches the requested resource.
func (cfd *CloudflaredMethod) Acquire(uri *url.URL, requrl, filename string) error {
	// Build our request
	req, err := cfd.BuildRequest(uri)
	if err != nil {
		cfd.mwriter.StartURI(requrl, "", 0, false)
		return err
	}

	resp, err := cfd.Client.Do(req)
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

	size := ParseContentLengthHeader(resp.Header)

	cfd.mwriter.StartURI(requrl, "", size, false)

	// Close the body at the end of the method
	defer resp.Body.Close()
	// We buffer up to 16kb at a time
	buffer := make([]byte, 1024*16)

	// We want to compute our different hashes, otherwise Apt will reject the package
	hashMD5 := md5.New()
	hashSHA1 := sha1.New()
	hashSHA256 := sha256.New()
	hashSHA512 := sha512.New()

	// And finally, we need to write to this file
	fp, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("Error opening file '%s': %v", filename, err)
	}

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			// Get a slice to just what was read
			bslice := buffer[:n]
			// Update our hashes
			hashMD5.Write(bslice)
			hashSHA1.Write(bslice)
			hashSHA256.Write(bslice)
			hashSHA512.Write(bslice)
			// Write to the file
			fp.Write(bslice)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("Error reading response body: %v", err)
		}
	}

	strMD5 := string(hashMD5.Sum(nil))
	strSHA1 := string(hashSHA1.Sum(nil))
	strSHA256 := string(hashSHA256.Sum(nil))
	strSHA512 := string(hashSHA512.Sum(nil))

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
	return nil
}
