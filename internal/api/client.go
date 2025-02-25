package api

import (
	"bytes"
	"context"
	"fmt"

	"github.com/putdotio/go-putio/putio"
	"golang.org/x/oauth2"
)

// Client wraps the official Put.io client
type Client struct {
	client *putio.Client
	ctx    context.Context
}

// NewClient creates a new Put.io API client
func NewClient(oauthToken string) *Client {
	ctx := context.Background()
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: oauthToken})
	oauthClient := oauth2.NewClient(ctx, tokenSource)

	return &Client{
		client: putio.NewClient(oauthClient),
		ctx:    ctx,
	}
}

// Authenticate verifies the OAuth token by fetching account info
func (c *Client) Authenticate() error {
	account, err := c.client.Account.Info(c.ctx)
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
func (c *Client) GetAccountInfo() (*putio.AccountInfo, error) {
	account, err := c.client.Account.Info(c.ctx)
	if err != nil {
		return nil, err
	}
	return &account, nil
}

// EnsureFolder creates a folder if it doesn't exist or returns the ID if it does
func (c *Client) EnsureFolder(name string) (int64, error) {
	// List files at root to find folder
	files, _, err := c.client.Files.List(c.ctx, 0)
	if err != nil {
		return 0, err
	}

	// Check if folder exists
	for _, file := range files {
		if file.Name == name {
			return file.ID, nil
		}
	}

	// Create folder if it doesn't exist
	folder, err := c.client.Files.CreateFolder(c.ctx, name, 0)
	if err != nil {
		return 0, err
	}

	return folder.ID, nil
}

// AddTransfer adds a new transfer (torrent) to Put.io
func (c *Client) AddTransfer(magnetLink string, folderID int64) error {
	transfer, err := c.client.Transfers.Add(c.ctx, magnetLink, folderID, "")
	if err != nil {
		return err
	}

	if transfer.Status == "ERROR" {
		return fmt.Errorf("transfer failed: %s", transfer.ErrorMessage)
	}

	return nil
}

// GetTransfers returns the list of current transfers
func (c *Client) GetTransfers() ([]*putio.Transfer, error) {
	transfers, err := c.client.Transfers.List(c.ctx)
	if err != nil {
		return nil, err
	}

	// Convert []putio.Transfer to []*putio.Transfer
	result := make([]*putio.Transfer, len(transfers))
	for i := range transfers {
		result[i] = &transfers[i]
	}
	return result, nil
}

// GetDownloadURL gets the download URL for a file
func (c *Client) GetDownloadURL(fileID int64) (string, error) {
	url, err := c.client.Files.URL(c.ctx, fileID, false)
	if err != nil {
		return "", err
	}
	return url, nil
}

// DeleteTransfer removes a transfer from Put.io
func (c *Client) DeleteTransfer(transferID int64) error {
	err := c.client.Transfers.Cancel(c.ctx, transferID)
	if err != nil {
		return err
	}
	return nil
}

// GetFiles gets the contents of a folder
func (c *Client) GetFiles(folderID int64) ([]*putio.File, error) {
	files, _, err := c.client.Files.List(c.ctx, folderID)
	if err != nil {
		return nil, err
	}

	// Convert []putio.File to []*putio.File
	result := make([]*putio.File, len(files))
	for i := range files {
		result[i] = &files[i]
	}
	return result, nil
}

// DeleteFile removes a file from Put.io
func (c *Client) DeleteFile(fileID int64) error {
	err := c.client.Files.Delete(c.ctx, fileID)
	if err != nil {
		return err
	}
	return nil
}

// UploadFile uploads a torrent file to Put.io
func (c *Client) UploadFile(data []byte, filename string, folderID int64) error {
	reader := bytes.NewReader(data)
	_, err := c.client.Files.Upload(c.ctx, reader, filename, folderID)
	if err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}
	return nil
}

// GetAllTransferFiles recursively gets all files in a transfer
func (c *Client) GetAllTransferFiles(fileID int64) ([]*putio.File, error) {
	// First check if the fileID is a file itself
	file, err := c.client.Files.Get(c.ctx, fileID)
	if err != nil {
		return nil, err
	}

	// If it's a single file, return it directly
	if !file.IsDir() {
		return []*putio.File{&file}, nil
	}

	// Otherwise, recursively get all files in the directory
	var allFiles []*putio.File
	var getFiles func(id int64) error

	getFiles = func(id int64) error {
		files, err := c.GetFiles(id)
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
