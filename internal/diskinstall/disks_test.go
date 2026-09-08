package diskinstall

import (
	"encoding/json"
	"testing"
)

func TestParseListDisks_Sorted(t *testing.T) {
	data := `{"disks":[
		{"path":"/dev/sdb","identity":"2","size_bytes":21474836480,"model":"M2","serial":"S2","transport":"sata","external":false,"system_eligible":true,"data_eligible":true},
		{"path":"/dev/sda","identity":"1","size_bytes":274877906944,"model":"M1","serial":"S1","transport":"nvme","external":false,"system_eligible":true,"data_eligible":true}
	]}`
	resp, err := ParseListDisks([]byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Disks) != 2 {
		t.Fatalf("len=%d want 2", len(resp.Disks))
	}
	if resp.Disks[0].Path != "/dev/sda" {
		t.Fatalf("first path=%q want /dev/sda (sorted by identity)", resp.Disks[0].Path)
	}
}

func TestParseListDisks_Empty(t *testing.T) {
	resp, err := ParseListDisks([]byte(`{"disks":[]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Disks) != 0 {
		t.Fatalf("want empty")
	}
}

func TestRecommendSystemDisk_InternalSmallest(t *testing.T) {
	disks := []Disk{
		{Path: "/dev/sda", Identity: "1", SizeBytes: 100 * 1024 * 1024 * 1024, SystemEligible: true, External: false},
		{Path: "/dev/sdb", Identity: "2", SizeBytes: 50 * 1024 * 1024 * 1024, SystemEligible: true, External: false},
		{Path: "/dev/sdc", Identity: "3", SizeBytes: 20 * 1024 * 1024 * 1024, SystemEligible: true, External: true},
	}
	rec := RecommendSystemDisk(disks, map[string]bool{})
	if rec == nil || rec.Path != "/dev/sdb" {
		t.Fatalf("recommend=%v want /dev/sdb", rec)
	}
	// With no internal, should pick smallest opted-in external
	disks2 := []Disk{
		{Path: "/dev/sda", Identity: "1", SizeBytes: 100 * 1024 * 1024 * 1024, SystemEligible: false, External: false},
		{Path: "/dev/sdc", Identity: "3", SizeBytes: 20 * 1024 * 1024 * 1024, SystemEligible: true, External: true},
		{Path: "/dev/sdd", Identity: "4", SizeBytes: 30 * 1024 * 1024 * 1024, SystemEligible: true, External: true},
	}
	rec = RecommendSystemDisk(disks2, map[string]bool{"/dev/sdc": true})
	if rec == nil || rec.Path != "/dev/sdc" {
		t.Fatalf("recommend external=%v want /dev/sdc", rec)
	}
	// No candidate without opt-in
	rec = RecommendSystemDisk(disks2, map[string]bool{})
	if rec != nil {
		t.Fatalf("want nil when no opted-in external, got %v", rec)
	}
}

func TestSelectedDisks_Order(t *testing.T) {
	disks := []Disk{
		{Path: "/dev/sda", Identity: "1", SizeBytes: 50 * 1024 * 1024 * 1024, SystemEligible: true, DataEligible: true, External: false},
		{Path: "/dev/sdb", Identity: "2", SizeBytes: 100 * 1024 * 1024 * 1024, SystemEligible: false, DataEligible: true, External: false},
		{Path: "/dev/sdc", Identity: "3", SizeBytes: 20 * 1024 * 1024 * 1024, SystemEligible: false, DataEligible: true, External: true},
	}
	sel := SelectedDisks(disks, "/dev/sda", map[string]bool{"/dev/sdc": true})
	if len(sel) != 3 {
		t.Fatalf("selected len=%d want 3", len(sel))
	}
	if sel[0].Path != "/dev/sda" {
		t.Fatalf("first must be system disk")
	}
	// Data disks sorted by identity
	if sel[1].Path != "/dev/sdb" || sel[2].Path != "/dev/sdc" {
		t.Fatalf("data order wrong: %v", sel)
	}
}

func TestErasePhrase(t *testing.T) {
	if got := ErasePhrase(2); got != "ERASE 2 DISKS" {
		t.Fatalf("phrase=%q", got)
	}
}

func TestNetworkFileRoundTrip(t *testing.T) {
	nf := NetworkFile{Mode: "wired-dhcp", ConnectionUUID: "", Interface: "", Keyfile: ""}
	data, _ := json.Marshal(nf)
	var out NetworkFile
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Mode != "wired-dhcp" {
		t.Fatalf("mode=%q", out.Mode)
	}
}
