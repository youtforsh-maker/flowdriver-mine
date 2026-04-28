package config

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/NullLatency/flow-driver/internal/httpclient"
)

func TestAppConfigSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	original := &AppConfig{
		ListenAddr:     "127.0.0.1:1080",
		ClientID:       "client-a",
		StorageType:    "google",
		LocalDir:       "tmp",
		GoogleFolderID: "folder-id",
		RefreshRateMs:  200,
		FlushRateMs:    300,
		Transport: httpclient.TransportConfig{
			TargetIP:           "216.239.38.120:443",
			SNI:                "google.com",
			HostHeader:         "www.googleapis.com",
			InsecureSkipVerify: false,
		},
	}

	if err := original.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !reflect.DeepEqual(loaded, original) {
		t.Fatalf("Load() = %#v, want %#v", loaded, original)
	}
}
