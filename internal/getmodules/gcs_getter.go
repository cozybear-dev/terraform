// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package getmodules

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	getter "github.com/hashicorp/go-getter"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcsGetter fetches module packages from Google Cloud Storage. It does the
// same thing as the getter of the same name in go-getter, and understands
// the same addresses, but is quicker for a configuration that uses many
// modules from buckets:
//
//   - All fetches share one storage client, where go-getter creates a new
//     one for every package. Each new client has to authenticate again and
//     open its own connections.
//   - The objects of a package that is a directory are downloaded
//     concurrently rather than one at a time.
type gcsGetter struct {
	client *getter.Client
}

var _ getter.Getter = (*gcsGetter)(nil)

// gcsGetterConcurrency is how many objects of one package are downloaded at
// the same time.
const gcsGetterConcurrency = 8

func (g *gcsGetter) SetClient(c *getter.Client) {
	g.client = c
}

func (g *gcsGetter) context() context.Context {
	if g.client == nil || g.client.Ctx == nil {
		return context.Background()
	}
	return g.client.Ctx
}

// mode returns the given file mode with the client's umask applied.
func (g *gcsGetter) mode(mode os.FileMode) os.FileMode {
	if g.client == nil {
		return mode
	}
	return mode & ^g.client.Umask
}

func (g *gcsGetter) ClientMode(u *url.URL) (getter.ClientMode, error) {
	ctx := g.context()

	bucket, object, _, err := parseGCSURL(u)
	if err != nil {
		return 0, err
	}
	client, err := sharedGCSClient()
	if err != nil {
		return 0, err
	}
	iter := client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: object})
	for {
		obj, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return 0, err
		}
		if strings.HasSuffix(obj.Name, "/") {
			// A directory matched the prefix search, so this must be a directory
			return getter.ClientModeDir, nil
		} else if obj.Name != object {
			// A file matched the prefix search and doesn't have the same name
			// as the query, so this must be a directory
			return getter.ClientModeDir, nil
		}
	}
	// There are no directories or subdirectories, and if a match was returned,
	// it was exactly equal to the prefix search. So return File mode
	return getter.ClientModeFile, nil
}

func (g *gcsGetter) Get(dst string, u *url.URL) error {
	ctx := g.context()

	bucket, object, _, err := parseGCSURL(u)
	if err != nil {
		return err
	}

	// Remove destination if it already exists
	_, err = os.Stat(dst)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}

	// Create all the parent directories
	if err := os.MkdirAll(filepath.Dir(dst), g.mode(0755)); err != nil {
		return err
	}

	client, err := sharedGCSClient()
	if err != nil {
		return err
	}

	// Find all of the matching objects first, and then download them
	// concurrently.
	type download struct {
		name, dst string
	}
	var downloads []download
	iter := client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: object})
	for {
		obj, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		if strings.HasSuffix(obj.Name, "/") {
			continue
		}
		objDst, err := filepath.Rel(object, obj.Name)
		if err != nil {
			return err
		}
		downloads = append(downloads, download{obj.Name, filepath.Join(dst, objDst)})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	next := make(chan download)
	for range min(gcsGetterConcurrency, len(downloads)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range next {
				if err := g.getObject(ctx, client, d.dst, bucket, d.name, ""); err != nil {
					errOnce.Do(func() {
						firstErr = err
						// There's no point in completing the others.
						cancel()
					})
				}
			}
		}()
	}
	for _, d := range downloads {
		next <- d
	}
	close(next)
	wg.Wait()
	return firstErr
}

func (g *gcsGetter) GetFile(dst string, u *url.URL) error {
	ctx := g.context()

	bucket, object, fragment, err := parseGCSURL(u)
	if err != nil {
		return err
	}
	client, err := sharedGCSClient()
	if err != nil {
		return err
	}
	return g.getObject(ctx, client, dst, bucket, object, fragment)
}

func (g *gcsGetter) getObject(ctx context.Context, client *storage.Client, dst, bucket, object, fragment string) error {
	handle := client.Bucket(bucket).Object(object)
	if fragment != "" {
		generation, err := strconv.ParseInt(fragment, 10, 64)
		if err != nil {
			return err
		}
		handle = handle.Generation(generation)
	}
	rc, err := handle.NewReader(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()

	// Create all the parent directories
	if err := os.MkdirAll(filepath.Dir(dst), g.mode(0755)); err != nil {
		return err
	}

	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	// Explicitly chmod; the process umask is unconditionally applied otherwise.
	return os.Chmod(dst, g.mode(0666))
}

// parseGCSURL finds the bucket, the object name and the optional generation
// in an address of the form
// https://www.googleapis.com/storage/v1/BUCKET/PATH#GENERATION.
func parseGCSURL(u *url.URL) (bucket, path, fragment string, err error) {
	if !strings.HasSuffix(u.Host, ".googleapis.com") {
		return "", "", "", fmt.Errorf("URL is not a valid GCS URL")
	}
	if hostParts := strings.Split(u.Host, "."); len(hostParts) != 3 {
		return "", "", "", fmt.Errorf("URL is not a valid GCS URL")
	}
	pathParts := strings.SplitN(u.Path, "/", 5)
	if len(pathParts) != 5 {
		return "", "", "", fmt.Errorf("URL is not a valid GCS URL")
	}
	return pathParts[3], pathParts[4], u.Fragment, nil
}

// gcsClients holds the storage client that all fetches share. A client is
// only kept for as long as the credentials it was created with are the
// ones that a new client would be given.
var gcsClients struct {
	sync.Mutex
	client   *storage.Client
	token    string
	hasToken bool
}

// sharedGCSClient returns the storage client to use, creating it if this is
// the first request. It is configured in the same way as go-getter
// configures the client it creates for each fetch: with the access token in
// GOOGLE_OAUTH_ACCESS_TOKEN if that's set, and otherwise with the
// application default credentials.
func sharedGCSClient() (*storage.Client, error) {
	token, hasToken := os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN")

	gcsClients.Lock()
	defer gcsClients.Unlock()
	if gcsClients.client != nil && gcsClients.token == token && gcsClients.hasToken == hasToken {
		return gcsClients.client, nil
	}

	var opts []option.ClientOption
	if hasToken {
		opts = append(opts, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: token,
		})))
	}
	// The client outlives the request that happens to create it, so it
	// mustn't be tied to that request's context.
	client, err := storage.NewClient(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	gcsClients.client, gcsClients.token, gcsClients.hasToken = client, token, hasToken
	return client, nil
}
