package client

import (
	"context"
	"encoding/json"
	"net/url"
)

// ImageLayers exports the locally stored metadata of the layers of an image.
//
// The daemon only reads local metadata; it does not pull missing layers or
// modify local images. Layers sharing a compressed digest are merged into a
// single entry while all source-repository references are preserved.
//
// This requires API version 1.57 or higher.
func (cli *Client) ImageLayers(ctx context.Context, imageID string, opts ...ImageLayersOption) (ImageLayersResult, error) {
	if imageID == "" {
		return ImageLayersResult{}, objectNotFoundError{object: "image", id: imageID}
	}

	if err := cli.requiresVersion(ctx, "1.57", "image layer metadata"); err != nil {
		return ImageLayersResult{}, err
	}

	var o imageLayersOpts
	for _, opt := range opts {
		if err := opt.Apply(&o); err != nil {
			return ImageLayersResult{}, err
		}
	}

	query := url.Values{}
	if o.platform != nil {
		p, err := encodePlatform(o.platform)
		if err != nil {
			return ImageLayersResult{}, err
		}
		query.Set("platform", p)
	}

	resp, err := cli.get(ctx, "/images/"+imageID+"/layers", query, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return ImageLayersResult{}, err
	}

	var result ImageLayersResult
	err = json.NewDecoder(resp.Body).Decode(&result.Items)
	return result, err
}
