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
