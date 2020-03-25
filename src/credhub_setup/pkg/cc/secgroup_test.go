package cc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func handleUnexpectedPath(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Unexpected HTTP %s on %s", r.Method, r.URL.String())
		t.Fail()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(fmt.Sprintf("Path %s is not found\n", r.URL.Path)))
	}
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	t.Parallel()

	bindingHandler := func(t *testing.T, groupGUID string, boundLifecycles map[lifecycleType]bool) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			pathParts := strings.FieldsFunc(req.URL.Path,
				func(r rune) bool { return r == '/' })
			if !assert.Lenf(t, pathParts, 4, "Unexpected request path %s", req.URL.Path) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if !assert.Equalf(t, groupGUID, pathParts[3], "Unexpected security group %s to bind", pathParts[3]) {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			var lifecycle lifecycleType
			switch pathParts[2] {
			case "staging_security_groups":
				lifecycle = lifecycleStaging
			case "running_security_groups":
				lifecycle = lifecycleRunning
			default:
				assert.Failf(t, "unknown lifecycle %s", pathParts[2])
				return
			}

			t.Logf("Got request for %s", lifecycle)
			boundLifecycles[lifecycle] = true
			w.WriteHeader(http.StatusNoContent)
		}
	}

	t.Run("creates a new security group", func(t *testing.T) {
		t.Parallel()

		builtGUID := "newly-created-security-group"

		boundLifecycles := map[lifecycleType]bool{}
		mux := http.NewServeMux()
		mux.Handle("/v2/config/", bindingHandler(t, builtGUID, boundLifecycles))
		server := httptest.NewServer(mux)
		defer server.Close()
		serverURL, err := url.Parse(server.URL)
		require.NoError(t, err, "failed to parse server URL")

		emptyGUID := ""
		builder := &SecurityGroupBuilder{
			Logger:          t,
			Client:          server.Client(),
			Endpoint:        serverURL,
			Name:            "new-security-group",
			Address:         serverURL.Hostname(),
			Ports:           serverURL.Port(),
			groupIDOverride: &emptyGUID,
		}
		builder.makeSecurityGroupRequest = func(ctx context.Context, guid, query, method string, body io.Reader) (string, error) {
			assert.Empty(t, guid, "unexpected non-empty GUID to create")
			assert.Equal(t, http.MethodPost, method, "unexpected method to create new security group")
			return builtGUID, nil
		}
		err = builder.Apply(ctx)
		assert.NoError(t, err, "unexpected error creating new security group")
		assert.Contains(t, boundLifecycles, lifecycleStaging, "staging not bound")
		assert.Contains(t, boundLifecycles, lifecycleRunning, "running not bound")
	})

	t.Run("updates an existing security group", func(t *testing.T) {
		t.Parallel()

		existingGUID := "existing-security-group"
		boundLifecycles := map[lifecycleType]bool{}
		mux := http.NewServeMux()
		mux.Handle("/v2/config/", bindingHandler(t, existingGUID, boundLifecycles))
		server := httptest.NewServer(mux)
		defer server.Close()
		serverURL, err := url.Parse(server.URL)
		require.NoError(t, err, "failed to parse server URL")

		builder := &SecurityGroupBuilder{
			Logger:          t,
			Client:          server.Client(),
			Endpoint:        serverURL,
			Name:            "existing-security-group",
			Address:         serverURL.Hostname(),
			Ports:           serverURL.Port(),
			groupIDOverride: &existingGUID,
		}
		builder.makeSecurityGroupRequest = func(ctx context.Context, guid, query, method string, body io.Reader) (string, error) {
			assert.Equal(t, existingGUID, guid, "unexpected GUID to update")
			assert.Equal(t, http.MethodPut, method, "unexpected method to update existing security group")
			return existingGUID, nil
		}
		err = builder.Apply(ctx)
		assert.NoError(t, err, "unexpected error updating existing security group")
		assert.Contains(t, boundLifecycles, lifecycleStaging, "staging not bound")
		assert.Contains(t, boundLifecycles, lifecycleRunning, "running not bound")
	})
}

func TestRemove(t *testing.T) {
	t.Parallel()

	t.Run("allows no groups to remove", func(t *testing.T) {
		t.Parallel()
		emptyGUID := ""
		builder := &SecurityGroupBuilder{
			Logger:          t,
			groupIDOverride: &emptyGUID,
		}
		builder.makeSecurityGroupRequest = func(ctx context.Context, guid, query, method string, body io.Reader) (string, error) {
			assert.Failf(t, "unexpected request", "trying to %s group %s", method, guid)
			return "", fmt.Errorf("test failed")
		}
		err := builder.Remove(context.Background())
		assert.NoError(t, err, "unexpected error removing no group")
	})

	t.Run("removes the desired group", func(t *testing.T) {
		t.Parallel()
		groupGUID := "some-group-to-be-removed"
		builder := &SecurityGroupBuilder{
			Logger:          t,
			groupIDOverride: &groupGUID,
		}
		builder.makeSecurityGroupRequest = func(ctx context.Context, guid, query, method string, body io.Reader) (string, error) {
			assert.Equal(t, http.MethodDelete, method, "unexpected method")
			assert.Equal(t, groupGUID, guid, "unexpected GUID")
			return "", nil
		}
		err := builder.Remove(context.Background())
		assert.NoError(t, err, "unexpected error removing group")
	})
}

func TestRequestor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	makeBuilder := func(t *testing.T) (*SecurityGroupBuilder, *http.ServeMux, chan<- bool, error) {
		cleanupWaiter := make(chan bool)
		mux := http.NewServeMux()
		mux.HandleFunc("/", handleUnexpectedPath(t))
		server := httptest.NewTLSServer(mux)
		go func() {
			<-cleanupWaiter
			server.Close()
		}()
		serverURL, err := url.Parse(server.URL)
		if err != nil {
			close(cleanupWaiter)
			return nil, nil, nil, fmt.Errorf("could not parse temporary server URL: %s", err)
		}
		builder := &SecurityGroupBuilder{
			Logger:   t,
			Client:   server.Client(),
			Endpoint: serverURL,
		}
		return builder, mux, cleanupWaiter, nil
	}

	t.Run("query for a group", func(t *testing.T) {
		t.Parallel()
		const expected = "desired-guid"

		builder, mux, cleanup, err := makeBuilder(t)
		defer close(cleanup)
		require.NoError(t, err, "could not create builder")

		query := url.Values{}
		query.Set("q", fmt.Sprintf("name:%s", builder.groupName()))
		mux.HandleFunc("/v2/security_groups", func(w http.ResponseWriter, r *http.Request) {
			if !assert.Equal(t, http.MethodGet, r.Method, "bad HTTP method") {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if !assert.Equal(t, query.Get("q"), r.FormValue("q")) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, err := io.WriteString(w, fmt.Sprintf(`{
				"resources": [
					{ "metadata": { "guid": "%s" }, "entity": { "name": "%s" } },
					{ "metadata": { "guid": "%s" }, "entity": { "name": "%s" } }
				]
			}`, "incorrect", "wrong name", expected, builder.groupName()))
			assert.NoError(t, err, "could not write response")
		})

		actual, err := builder.defaultRequester(ctx, "", query.Encode(), http.MethodGet, nil)
		assert.NoError(t, err, "unexpected error running query")
		assert.Equal(t, expected, actual, "unepxected id")
	})

	t.Run("create a group", func(t *testing.T) {
		t.Parallel()
		const expected = "group-guid"
		const contents = "body contents"

		builder, mux, cleanup, err := makeBuilder(t)
		defer close(cleanup)
		require.NoError(t, err, "could not create builder")

		mux.HandleFunc("/v2/security_groups", func(w http.ResponseWriter, r *http.Request) {
			if !assert.Equal(t, http.MethodPost, r.Method, "bad HTTP method") {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			body, err := ioutil.ReadAll(r.Body)
			if !assert.NoError(t, err, "could not read request body") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !assert.Equal(t, contents, string(body), "unexpected request body") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, err = io.WriteString(w, fmt.Sprintf(`{
				"metadata": { "guid": "%s" }, "entity": { "name": "%s" }
			}`, expected, "group-name"))
			assert.NoError(t, err, "failed to write response")
		})

		body := bytes.NewBufferString(contents)
		actual, err := builder.defaultRequester(ctx, "", "", http.MethodPost, body)
		assert.NoError(t, err, "could not make request")
		assert.Equal(t, expected, actual, "unexpected group GUID")
	})

	t.Run("update a group", func(t *testing.T) {
		t.Parallel()
		const (
			guid    = "group-guid"
			newName = "new-name"
		)
		expectedBody := fmt.Sprintf(`{ "name": "%s" }`, newName)

		builder, mux, cleanup, err := makeBuilder(t)
		defer close(cleanup)
		require.NoError(t, err, "could not create builder")

		executedUpdate := false
		mux.HandleFunc("/v2/security_groups/"+guid, func(w http.ResponseWriter, r *http.Request) {
			if !assert.Equal(t, http.MethodPut, r.Method, "unexpected method") {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			body, err := ioutil.ReadAll(r.Body)
			if !assert.NoError(t, err, "could not read request body") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !assert.Equal(t, expectedBody, string(body), "unexpected request body") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			executedUpdate = true
			w.WriteHeader(http.StatusOK)
			_, err = io.WriteString(w, fmt.Sprintf(`{
				"metadata": { "guid": "%s" }, "entity": { "name": "%s" }
			}`, guid, newName))
			assert.NoError(t, err, "failed to write response")
		})

		body := bytes.NewBufferString(expectedBody)
		actual, err := builder.defaultRequester(ctx, guid, "", http.MethodPut, body)
		assert.NoError(t, err, "error updating security group")
		assert.Equal(t, guid, actual)
		assert.True(t, executedUpdate, "did not execute update")
	})

	t.Run("delete a group", func(t *testing.T) {
		t.Parallel()
		const (
			existingGUID = "existing-guid"
			missingGUID  = "missing-guid"
		)

		deleted := map[string]bool{}
		deletedMut := sync.Mutex{}
		wg := sync.WaitGroup{}
		wg.Add(1)
		defer wg.Done()
		builder, mux, cleanup, err := makeBuilder(t)
		go func() {
			defer close(cleanup)
			wg.Wait()
			assert.Contains(t, deleted, existingGUID)
			assert.Contains(t, deleted, missingGUID)
		}()
		require.NoError(t, err, "could not create builder")

		mux.HandleFunc("/v2/security_groups/"+existingGUID, func(w http.ResponseWriter, r *http.Request) {
			if !assert.Equal(t, http.MethodDelete, r.Method, "unexpected method") {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			deletedMut.Lock()
			deleted[existingGUID] = true
			deletedMut.Unlock()
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("/v2/security_groups/"+missingGUID, func(w http.ResponseWriter, r *http.Request) {
			if !assert.Equal(t, http.MethodDelete, r.Method, "unexpected method") {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			deletedMut.Lock()
			deleted[missingGUID] = true
			deletedMut.Unlock()
			w.WriteHeader(http.StatusNotFound)
		})

		t.Run("sucessfully", func(t *testing.T) {
			wg.Add(1)
			defer wg.Done()
			t.Parallel()
			_, err := builder.defaultRequester(ctx, existingGUID, "", http.MethodDelete, nil)
			assert.NoError(t, err, "failed to delete existing GUID")
		})
		t.Run("when the group is missing", func(t *testing.T) {
			wg.Add(1)
			defer wg.Done()
			t.Parallel()
			_, err := builder.defaultRequester(ctx, missingGUID, "", http.MethodDelete, nil)
			assert.NoError(t, err, "failed to delete missing GUID")
		})
	})
	assert.NotNil(t, makeBuilder)
}

func TestGroupID(t *testing.T) {
	t.Run("when the group exists", func(t *testing.T) {
		const expected = "some-group-id"
		builder := &SecurityGroupBuilder{
			groupNameCache: "group-name",
			makeSecurityGroupRequest: func(ctx context.Context, guid, query, method string, body io.Reader) (string, error) {
				if !assert.Equal(t, http.MethodGet, method) {
					return "", fmt.Errorf("incorrect method")
				}
				if !assert.Empty(t, guid, "group ID already known") {
					return "", fmt.Errorf("incorrect parameters")
				}
				values, err := url.ParseQuery(query)
				if !assert.NoError(t, err, "could not parse query") {
					return "", err
				}
				assert.Equal(t, "name:group-name", values.Get("q"), "unexpected query")
				return expected, nil
			},
		}
		id, err := builder.groupID(context.Background())
		assert.NoError(t, err, "unexpected error getting group ID")
		assert.Equal(t, expected, id, "unexpected group ID")
	})

	t.Run("when the group is missing", func(t *testing.T) {
		builder := &SecurityGroupBuilder{
			groupNameCache: "group-name",
			makeSecurityGroupRequest: func(ctx context.Context, guid, query, method string, body io.Reader) (string, error) {
				if !assert.Equal(t, http.MethodGet, method) {
					return "", fmt.Errorf("incorrect method")
				}
				if !assert.Empty(t, guid, "group ID already known") {
					return "", fmt.Errorf("incorrect parameters")
				}
				values, err := url.ParseQuery(query)
				if !assert.NoError(t, err, "could not parse query") {
					return "", err
				}
				assert.Equal(t, "name:group-name", values.Get("q"), "unexpected query")
				return "", nil
			},
		}
		id, err := builder.groupID(context.Background())
		assert.NoError(t, err, "unexpected error getting group ID")
		assert.Empty(t, id, "unexpected group ID")
	})
}