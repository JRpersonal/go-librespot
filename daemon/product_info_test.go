//go:build test_unit

package daemon

import (
	"encoding/xml"
	"testing"

	"github.com/devgianlu/go-librespot/ap"
	"github.com/stretchr/testify/require"
)

func TestProductTypeUnknownIsNil(t *testing.T) {
	require.Nil(t, productType(nil), "no ProductInfo packet yet")
	require.Nil(t, productType(&ProductInfo{}), "packet without products")

	var blank ProductInfo
	blank.Products = append(blank.Products, struct {
		XMLName      xml.Name `xml:"product"`
		Type         string   `xml:"type"`
		HeadFilesUrl string   `xml:"head-files-url"`
		ImageUrl     string   `xml:"image-url"`
	}{Type: "  "})
	require.Nil(t, productType(&blank), "blank type")
}

func TestProductTypeFromAccesspointPacket(t *testing.T) {
	p := &AppPlayer{}
	require.Nil(t, productType(p.prodInfo))

	payload := []byte(`<products><product><type>Premium</type>` +
		`<head-files-url>https://example.com/head/{file_id}</head-files-url>` +
		`<image-url>https://example.com/image/{file_id}</image-url></product></products>`)
	require.NoError(t, p.handleAccesspointPacket(ap.PacketTypeProductInfo, payload))

	got := productType(p.prodInfo)
	require.NotNil(t, got)
	require.Equal(t, "premium", *got)

	// A fresh session (new AppPlayer, as after a zeroconf account switch)
	// starts without the previous account's product type.
	next := &AppPlayer{}
	require.Nil(t, productType(next.prodInfo))
}
