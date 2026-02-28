package api

import (
	"bytes"
	"context"
	"fmt"

	"github.com/elsbrock/go-putio"
	"golang.org/x/oauth2"
)

// Client wraps the official Put.io client
type Client struct {
	client *putio.Client
}

// NewClient creates a new Put.io API client
func NewClient(oauthToken string) *Client {
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: oauthToken})
	oauthClient := oauth2.NewClient(context.Background(), tokenSource)

	return &Client{
		client: putio.NewClient(oauthClient),
	}
}

// Authenticate verifies the OAuth token by fetching account info
func (c *Client) Authenticate(ctx context.Context) error {
	account, err := c.client.Account.Info(ctx)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Just verify we got a valid user ID
	if account.Username == "" {
		return fmt.Errorf("invalid account info received")
	}

	return nil
}

// GetAccountInfo returns the Put.io account information
func (c *Client) GetAccountInfo(ctx context.Context) (*putio.AccountInfo, error) {
	account, err := c.client.Account.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("get account info: %w", err)
	}
	return &account, nil
}

// EnsureFolder creates a folder if it doesn't exist or returns the ID if it does
func (c *Client) EnsureFolder(ctx context.Context, name string) (int64, error) {
	// List files at root to find folder
	files, _, err := c.client.Files.List(ctx, 0)
	if err != nil {
		return 0, fmt.Errorf("ensure folder: %w", err)
	}

	// Check if folder exists
	for _, file := range files {
		if file.Name == name {
			return file.ID, nil
		}
	}

	// Create folder if it doesn't exist
	folder, err := c.client.Files.CreateFolder(ctx, name, 0)
	if err != nil {
		return 0, fmt.Errorf("ensure folder: %w", err)
	}

	return folder.ID, nil
}

// AddTransfer adds a new transfer (torrent) to Put.io and returns its hash.
func (c *Client) AddTransfer(ctx context.Context, magnetLink string, folderID int64) (string, error) {
	transfer, err := c.client.Transfers.Add(ctx, magnetLink, folderID, "")
	if err != nil {
		return "", fmt.Errorf("add transfer: %w", err)
	}

	if transfer.Status == "ERROR" {
		return "", fmt.Errorf("transfer failed: %s", transfer.ErrorMessage)
	}

	return transfer.Hash, nil
}

// GetTransfers returns the list of current transfers
func (c *Client) GetTransfers(ctx context.Context) ([]*putio.Transfer, error) {
	transfers, err := c.client.Transfers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("get transfers: %w", err)
	}

	// Convert []putio.Transfer to []*putio.Transfer
	result := make([]*putio.Transfer, len(transfers))
	for i := range transfers {
		result[i] = &transfers[i]
	}
	return result, nil
}

// GetDownloadURL gets the download URL for a file
func (c *Client) GetDownloadURL(ctx context.Context, fileID int64) (string, error) {
	url, err := c.client.Files.URL(ctx, fileID, false)
	if err != nil {
		return "", fmt.Errorf("get download URL: %w", err)
	}
	return url, nil
}

// DeleteTransfer removes a transfer from Put.io
func (c *Client) DeleteTransfer(ctx context.Context, transferID int64) error {
	if err := c.client.Transfers.Cancel(ctx, transferID); err != nil {
		return fmt.Errorf("cancel transfer: %w", err)
	}
	return nil
}

// GetFiles gets the contents of a folder
func (c *Client) GetFiles(ctx context.Context, folderID int64) ([]*putio.File, error) {
	files, _, err := c.client.Files.List(ctx, folderID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}

	// Convert []putio.File to []*putio.File
	result := make([]*putio.File, len(files))
	for i := range files {
		result[i] = &files[i]
	}
	return result, nil
}

// DeleteFile removes a file from Put.io
func (c *Client) DeleteFile(ctx context.Context, fileID int64) error {
	if err := c.client.Files.Delete(ctx, fileID); err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	return nil
}

// UploadFile uploads a torrent file to Put.io and returns the transfer hash
// if one was created.
func (c *Client) UploadFile(ctx context.Context, data []byte, filename string, folderID int64) (string, error) {
	reader := bytes.NewReader(data)
	upload, err := c.client.Files.Upload(ctx, reader, filename, folderID)
	if err != nil {
		return "", fmt.Errorf("failed to upload file: %w", err)
	}
	if upload.Transfer != nil {
		return upload.Transfer.Hash, nil
	}
	return "", nil
}

// GetAllTransferFiles recursively gets all files in a transfer
func (c *Client) GetAllTransferFiles(ctx context.Context, fileID int64) ([]*putio.File, error) {
	// First check if the fileID is a file itself
	file, err := c.client.Files.Get(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("get transfer files: %w", err)
	}

	// If it's a single file, return it directly
	if !file.IsDir() {
		return []*putio.File{&file}, nil
	}

	// Otherwise, recursively get all files in the directory
	var allFiles []*putio.File
	var getFiles func(id int64) error

	getFiles = func(id int64) error {
		files, err := c.GetFiles(ctx, id)
		if err != nil {
			return err
		}

		for _, file := range files {
			if file.IsDir() {
				if err := getFiles(file.ID); err != nil {
					return err
				}
			} else {
				allFiles = append(allFiles, file)
			}
		}
		return nil
	}

	if err := getFiles(fileID); err != nil {
		return nil, err
	}

	return allFiles, nil
}

// RetryTransfer retries a failed transfer
func (c *Client) RetryTransfer(ctx context.Context, transferID int64) (*putio.Transfer, error) {
	transfer, err := c.client.Transfers.Retry(ctx, transferID)
	if err != nil {
		return nil, fmt.Errorf("failed to retry transfer: %w", err)
	}
	return &transfer, nil
}
