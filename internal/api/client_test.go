package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/elsbrock/go-putio"
)

func TestTransferRootNotFoundIsDistinctFromMissingChild(t *testing.T) {
	for _, missingRoot := range []bool{true, false} {
		t.Run(fmt.Sprint(missingRoot), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !missingRoot && r.URL.Path == "/v2/files/500" {
					fmt.Fprint(w, `{"status":"OK","file":{"id":500,"name":"Book","content_type":"application/x-directory","file_type":"FOLDER"}}`)
					return
				}
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"status":"ERROR","error_type":"NotFound","error_message":"missing"}`)
			}))
			defer srv.Close()
			client := putio.NewClient(srv.Client())
			client.BaseURL, _ = url.Parse(srv.URL)
			_, err := (&Client{client: client}).GetAllTransferFiles(context.Background(), 500)
			var rootErr *TransferSourceNotFoundError
			if err == nil || errors.As(err, &rootErr) != missingRoot {
				t.Fatalf("root missing=%v, got %v", missingRoot, err)
			}
			var response *putio.ErrorResponse
			if !errors.As(err, &response) || response.Type != "NotFound" {
				t.Fatalf("must retain underlying API error: %v", err)
			}
		})
	}
}

func TestIsNotFoundUnwrapsClientErrors(t *testing.T) {
	notFound := &putio.ErrorResponse{Type: "NotFound"}

	if !IsNotFound(notFound) {
		t.Fatal("expected direct Put.io NotFound error to be recognized")
	}
	if !IsNotFound(fmt.Errorf("get transfer files: %w", notFound)) {
		t.Fatal("expected wrapped Put.io NotFound error to be recognized")
	}
	if !IsNotFound(fmt.Errorf("process transfer: %w", fmt.Errorf("get transfer files: %w", notFound))) {
		t.Fatal("expected doubly wrapped Put.io NotFound error to be recognized")
	}
	if IsNotFound(&putio.ErrorResponse{Type: "Other"}) {
		t.Fatal("did not expect a different Put.io error type to match")
	}
	if IsNotFound(errors.New("not found")) {
		t.Fatal("did not expect an unrelated error to match")
	}
}
