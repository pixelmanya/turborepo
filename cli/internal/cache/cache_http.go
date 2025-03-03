// Adapted from https://github.com/thought-machine/please
// Copyright Thought Machine, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
package cache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	log "log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/vercel/turborepo/cli/internal/analytics"
	"github.com/vercel/turborepo/cli/internal/config"
	"github.com/vercel/turborepo/cli/internal/fs"
)

type client interface {
	PutArtifact(hash string, body []byte, duration int, tag string) error
	FetchArtifact(hash string) (*http.Response, error)
}

type httpCache struct {
	writable       bool
	client         client
	requestLimiter limiter
	recorder       analytics.Recorder
	signerVerifier *ArtifactSignatureAuthentication
	repoRoot       fs.AbsolutePath
}

type limiter chan struct{}

func (l limiter) acquire() {
	l <- struct{}{}
}

func (l limiter) release() {
	<-l
}

// mtime is the time we attach for the modification time of all files.
var mtime = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// nobody is the usual uid / gid of the 'nobody' user.
const nobody = 65534

func (cache *httpCache) Put(target, hash string, duration int, files []string) error {
	// if cache.writable {
	cache.requestLimiter.acquire()
	defer cache.requestLimiter.release()

	r, w := io.Pipe()
	go cache.write(w, hash, files)

	// Read the entire artifact tar into memory so we can easily compute the signature.
	// Note: retryablehttp.NewRequest reads the files into memory anyways so there's no
	// additional overhead by doing the ioutil.ReadAll here instead.
	artifactBody, err := ioutil.ReadAll(r)
	if err != nil {
		return fmt.Errorf("failed to store files in HTTP cache: %w", err)
	}
	tag := ""
	if cache.signerVerifier.isEnabled() {
		tag, err = cache.signerVerifier.generateTag(hash, artifactBody)
		if err != nil {
			return fmt.Errorf("failed to store files in HTTP cache: %w", err)
		}
	}
	return cache.client.PutArtifact(hash, artifactBody, duration, tag)
}

// write writes a series of files into the given Writer.
func (cache *httpCache) write(w io.WriteCloser, hash string, files []string) {
	defer w.Close()
	gzw := gzip.NewWriter(w)
	defer gzw.Close()
	tw := tar.NewWriter(gzw)
	defer tw.Close()
	for _, file := range files {
		// log.Printf("caching file %v", file)
		if err := cache.storeFile(tw, file); err != nil {
			log.Printf("[ERROR] Error uploading artifact %s to HTTP cache due to: %s", file, err)
			// TODO(jaredpalmer): How can we cancel the request at this point?
		}
	}
}

func (cache *httpCache) storeFile(tw *tar.Writer, repoRelativePath string) error {
	info, err := os.Lstat(repoRelativePath)
	if err != nil {
		return err
	}
	target := ""
	if info.Mode()&os.ModeSymlink != 0 {
		target, err = os.Readlink(repoRelativePath)
		if err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(info, filepath.ToSlash(target))
	if err != nil {
		return err
	}
	// Ensure posix path for filename written in header.
	hdr.Name = filepath.ToSlash(repoRelativePath)
	// Zero out all timestamps.
	hdr.ModTime = mtime
	hdr.AccessTime = mtime
	hdr.ChangeTime = mtime
	// Strip user/group ids.
	hdr.Uid = nobody
	hdr.Gid = nobody
	hdr.Uname = "nobody"
	hdr.Gname = "nobody"
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	} else if info.IsDir() || target != "" {
		return nil // nothing to write
	}
	f, err := os.Open(repoRelativePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	if errors.Is(err, tar.ErrWriteTooLong) {
		log.Printf("Error writing %v to tar file, info: %v, mode: %v, is regular: %v", repoRelativePath, info, info.Mode(), info.Mode().IsRegular())
	}
	return err
}

func (cache *httpCache) Fetch(target, key string, _unusedOutputGlobs []string) (bool, []string, int, error) {
	cache.requestLimiter.acquire()
	defer cache.requestLimiter.release()
	hit, files, duration, err := cache.retrieve(key)
	if err != nil {
		// TODO: analytics event?
		return false, files, duration, fmt.Errorf("failed to retrieve files from HTTP cache: %w", err)
	}
	cache.logFetch(hit, key, duration)
	return hit, files, duration, err
}

func (cache *httpCache) logFetch(hit bool, hash string, duration int) {
	var event string
	if hit {
		event = cacheEventHit
	} else {
		event = cacheEventMiss
	}
	payload := &CacheEvent{
		Source:   "REMOTE",
		Event:    event,
		Hash:     hash,
		Duration: duration,
	}
	cache.recorder.LogEvent(payload)
}

func (cache *httpCache) retrieve(hash string) (bool, []string, int, error) {
	resp, err := cache.client.FetchArtifact(hash)
	if err != nil {
		return false, nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil, 0, nil // doesn't exist - not an error
	} else if resp.StatusCode != http.StatusOK {
		b, _ := ioutil.ReadAll(resp.Body)
		return false, nil, 0, fmt.Errorf("%s", string(b))
	}
	// If present, extract the duration from the response.
	duration := 0
	if resp.Header.Get("x-artifact-duration") != "" {
		intVar, err := strconv.Atoi(resp.Header.Get("x-artifact-duration"))
		if err != nil {
			return false, nil, 0, fmt.Errorf("invalid x-artifact-duration header: %w", err)
		}
		duration = intVar
	}
	var tarReader io.Reader

	defer func() { _ = resp.Body.Close() }()
	if cache.signerVerifier.isEnabled() {
		expectedTag := resp.Header.Get("x-artifact-tag")
		if expectedTag == "" {
			// If the verifier is enabled all incoming artifact downloads must have a signature
			return false, nil, 0, errors.New("artifact verification failed: Downloaded artifact is missing required x-artifact-tag header")
		}
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return false, nil, 0, fmt.Errorf("artifact verifcation failed: %w", err)
		}
		isValid, err := cache.signerVerifier.validate(hash, b, expectedTag)
		if err != nil {
			return false, nil, 0, fmt.Errorf("artifact verifcation failed: %w", err)
		}
		if !isValid {
			err = fmt.Errorf("artifact verification failed: artifact tag does not match expected tag %s", expectedTag)
			return false, nil, 0, err
		}
		// The artifact has been verified and the body can be read and untarred
		tarReader = bytes.NewReader(b)
	} else {
		tarReader = resp.Body
	}
	files, err := restoreTar(cache.repoRoot, tarReader)
	if err != nil {
		return false, nil, 0, err
	}
	return true, files, duration, nil
}

// restoreTar returns posix-style repo-relative paths of the files it
// restored. In the future, these should likely be repo-relative system paths
// so that they are suitable for being fed into cache.Put for other caches.
// For now, I think this is working because windows also accepts /-delimited paths.
func restoreTar(root fs.AbsolutePath, reader io.Reader) ([]string, error) {
	files := []string{}
	missingLinks := []*tar.Header{}
	gzr, err := gzip.NewReader(reader)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				for _, link := range missingLinks {
					err := restoreSymlink(root, link, true)
					if err != nil {
						return nil, err
					}
				}

				return files, nil
			}
			return nil, err
		}
		// hdr.Name is always a posix-style path
		// TODO: files should eventually be repo-relative system paths
		files = append(files, hdr.Name)
		filename := root.Join(hdr.Name)
		if isChild, err := root.ContainsPath(filename); err != nil {
			return nil, err
		} else if !isChild {
			return nil, fmt.Errorf("cannot untar file to %v", filename)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := filename.MkdirAll(); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if dir := filename.Dir(); dir != "." {
				if err := dir.MkdirAll(); err != nil {
					return nil, err
				}
			}
			if f, err := filename.OpenFile(os.O_WRONLY|os.O_TRUNC|os.O_CREATE, os.FileMode(hdr.Mode)); err != nil {
				return nil, err
			} else if _, err := io.Copy(f, tr); err != nil {
				return nil, err
			} else if err := f.Close(); err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			if err := restoreSymlink(root, hdr, false); errors.Is(err, errNonexistentLinkTarget) {
				missingLinks = append(missingLinks, hdr)
			} else if err != nil {
				return nil, err
			}
		default:
			log.Printf("Unhandled file type %d for %s", hdr.Typeflag, hdr.Name)
		}
	}
}

var errNonexistentLinkTarget = errors.New("the link target does not exist")

func restoreSymlink(root fs.AbsolutePath, hdr *tar.Header, allowNonexistentTargets bool) error {
	// Note that hdr.Linkname is really the link target
	relativeLinkTarget := filepath.FromSlash(hdr.Linkname)
	linkFilename := root.Join(hdr.Name)
	if err := linkFilename.EnsureDir(); err != nil {
		return err
	}

	// TODO: check if this is an absolute path, or if we even care
	linkTarget := linkFilename.Dir().Join(relativeLinkTarget)
	if _, err := linkTarget.Lstat(); err != nil {
		if os.IsNotExist(err) {
			if !allowNonexistentTargets {
				return errNonexistentLinkTarget
			}
			// if we're allowing nonexistent link targets, proceed to creating the link
		} else {
			return err
		}
	}
	// Ensure that the link we're about to create doesn't already exist
	if err := linkFilename.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := linkFilename.Symlink(relativeLinkTarget); err != nil {
		return err
	}
	return nil
}

func (cache *httpCache) Clean(target string) {
	// Not possible; this implementation can only clean for a hash.
}

func (cache *httpCache) CleanAll() {
	// Also not possible.
}

func (cache *httpCache) Shutdown() {}

func newHTTPCache(opts Opts, config *config.Config, recorder analytics.Recorder) *httpCache {
	return &httpCache{
		writable:       true,
		client:         config.ApiClient,
		requestLimiter: make(limiter, 20),
		recorder:       recorder,
		signerVerifier: &ArtifactSignatureAuthentication{
			// TODO(Gaspar): this should use RemoteCacheOptions.TeamId once we start
			// enforcing team restrictions for repositories.
			teamId:  config.TeamId,
			enabled: config.TurboJSON.RemoteCacheOptions.Signature,
		},
		repoRoot: config.Cwd,
	}
}
