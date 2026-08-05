package service

import (
	"testing"

	"spark/model"
)

func TestFilterImagesAvailableByNodes(t *testing.T) {
	image := func(name string, nodeImages map[string]string) model.Image {
		return model.Image{Name: name, NodeImages: nodeImages}
	}

	tests := []struct {
		name   string
		images []model.Image
		nodes  []string
		want   []string
	}{
		{
			name:   "all nodes present -> available",
			images: []model.Image{image("debian-12-cloud", map[string]string{"node1": "/templates/debian.qcow2", "node2": "/templates/debian.qcow2"})},
			nodes:  []string{"node1", "node2"},
			want:   []string{"debian-12-cloud"},
		},
		{
			name:   "partial node presence -> not available",
			images: []model.Image{image("debian-12-cloud", map[string]string{"node1": "/templates/debian.qcow2"})},
			nodes:  []string{"node1", "node2"},
			want:   nil,
		},
		{
			name:   "empty node list -> empty result",
			images: []model.Image{image("debian-12-cloud", map[string]string{"node1": "/templates/debian.qcow2"})},
			nodes:  nil,
			want:   nil,
		},
		{
			name:   "missing node_images map -> not available",
			images: []model.Image{image("centos-9-cloud", nil)},
			nodes:  []string{"node1"},
			want:   nil,
		},
		{
			name: "mixed images",
			images: []model.Image{
				image("debian-12-cloud", map[string]string{"node1": "a", "node2": "b"}),
				image("ubuntu-24-cloud", map[string]string{"node1": "a"}),
				image("centos-9-cloud", map[string]string{"node1": "a", "node2": "b"}),
			},
			nodes: []string{"node1", "node2"},
			want:  []string{"debian-12-cloud", "centos-9-cloud"},
		},
		{
			name:   "node present with empty value still counts",
			images: []model.Image{image("debian-12-cloud", map[string]string{"node1": ""})},
			nodes:  []string{"node1"},
			want:   []string{"debian-12-cloud"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterImagesAvailableByNodes(tt.images, tt.nodes)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d images, want %d", len(got), len(tt.want))
			}
			for i, img := range got {
				if img.Name != tt.want[i] {
					t.Errorf("image %d: got %q, want %q", i, img.Name, tt.want[i])
				}
			}
		})
	}
}

// TestSlicePage pins down the shared Go-side page slicing used by the zone
// image list: the slice never runs past the end, an offset past the end
// yields an empty result, and a limit of 0 yields an empty page. Negative
// limit/offset are clamped to 0 (the HTTP layer rejects them, but the shared
// helper must not panic or mis-slice for callers that pass them).
func TestSlicePage(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	cases := []struct {
		name       string
		limit, off int
		want       []int
	}{
		{"first page", 2, 0, []int{1, 2}},
		{"middle page", 2, 2, []int{3, 4}},
		{"last short page", 2, 4, []int{5}},
		{"offset past the end", 2, 10, []int{}},
		{"limit 0", 0, 0, []int{}},
		{"exact end", 5, 0, []int{1, 2, 3, 4, 5}},
		{"negative limit clamps to 0", -1, 0, []int{}},
		{"negative offset clamps to 0", 2, -3, []int{1, 2}},
		{"both negative", -1, -1, []int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slicePage(items, tc.limit, tc.off)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
