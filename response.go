package grab

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// Response represents the response to a completed or in-process download
// request.
//
// For asyncronous operations, the Response also provides context for the file
// transfer while it is process. All functions are safe to use from multiple
// go-routines.
type Response struct {
	// The Request that was sent to obtain this Response.
	Request *Request

	// HTTPResponse specifies the HTTP response received from the remote server.
	// The response's Body is nil (having already been consumed).
	HTTPResponse *http.Response

	// Filename specifies the path where the file transfer is stored in local
	// storage.
	Filename string

	// Size specifies the total size of the file transfer.
	Size uint64

	// Error specifies any error that may have occurred during the file transfer
	// that created this response.
	Error error

	// Start specifies the time at which the file transfer started.
	Start time.Time

	// End specifies the time at which the file transfer completed.
	End time.Time

	// DidResume specifies that the file transfer resumed a previously
	// incomplete transfer.
	DidResume bool

	// writer is the file handle used to write the downloaded file to local
	// storage
	writer io.WriteCloser

	// bytesTransferred specifies the number of bytes which have already been
	// transferred and should only be accessed atomically.
	bytesTransferred uint64

	// doneFlag is incremented once the transfer is finalized, either
	// successfully or with errors.
	doneFlag int32
}

// IsComplete indicates whether the Response transfer context has completed with
// either a success or failure.
func (c *Response) IsComplete() bool {
	return atomic.LoadInt32(&c.doneFlag) > 0
}

// BytesTransferred returns the number of bytes which have already been
// downloaded.
func (c *Response) BytesTransferred() uint64 {
	return atomic.LoadUint64(&c.bytesTransferred)
}

// Progress returns the ratio of bytes which have already been downloaded over
// the total content length as a fraction of 1.00.
func (c *Response) Progress() float64 {
	if c.Size == 0 {
		return 0
	}

	return float64(atomic.LoadUint64(&c.bytesTransferred)) / float64(c.Size)
}

// Duration returns the duration of a file transfer. If the transfer is in
// process, the duration will be between now and the start of the transfer. If
// the transfer is complete, the duration will be between the start and end of
// the completed transfer process.
func (c *Response) Duration() time.Duration {
	if c.IsComplete() {
		return c.End.Sub(c.Start)
	} else {
		return time.Now().Sub(c.Start)
	}
}

// AverageBytesPerSecond returns the average bytes transferred per second over
// the duration of the file transfer.
func (c *Response) AverageBytesPerSecond() float64 {
	return float64(c.BytesTransferred()) / c.Duration().Seconds()
}

// copy transfers content for a HTTP connection established via Client.do()
func (c *Response) copy() error {
	// close writer when finished
	defer c.writer.Close()

	// download and update progress
	var buffer [4096]byte
	complete := false
	for complete == false {
		// read HTTP stream
		n, err := c.HTTPResponse.Body.Read(buffer[:])
		if err != nil && err != io.EOF {
			return c.close(err)
		}

		// write to file
		if _, werr := c.writer.Write(buffer[:n]); werr != nil {
			return c.close(werr)
		}

		// increment progress
		atomic.AddUint64(&c.bytesTransferred, uint64(n))

		// break when finished
		if err == io.EOF {
			// download is ready for checksum validation
			c.writer.Close()
			complete = true
		}
	}

	// validate checksum
	if complete && c.Request.Hash != nil && c.Request.Checksum != nil {
		// open downloaded file
		if f, err := os.Open(c.Filename); err != nil {
			return c.close(err)
		} else {
			defer f.Close()

			// hash file
			if _, err := io.Copy(c.Request.Hash, f); err != nil {
				return c.close(err)
			}

			// compare checksum
			sum := c.Request.Hash.Sum(nil)
			if !bytes.Equal(sum, c.Request.Checksum) {
				// delete file
				if c.Request.RemoveOnError {
					f.Close()
					os.Remove(c.Filename)
				}

				return c.close(newGrabError(errChecksumMismatch, "Checksum mismatch: %v", hex.EncodeToString(sum)))
			}
		}
	}

	return c.close(nil)
}

// close finalizes the response context
func (c *Response) close(err error) error {
	// close any file handle
	if c.writer != nil {
		c.writer.Close()
		c.writer = nil
	}

	// set result error (if any)
	c.Error = err

	// stop time
	c.End = time.Now()

	// set done flag
	atomic.AddInt32(&c.doneFlag, 1)

	// notify
	if c.Request.notifyOnCloseInternal != nil {
		c.Request.notifyOnCloseInternal <- c
	}

	if c.Request.NotifyOnClose != nil {
		c.Request.NotifyOnClose <- c
	}

	// pass error back to caller
	return err
}
