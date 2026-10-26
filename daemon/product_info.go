package daemon

import (
	"encoding/hex"
	"encoding/xml"
	"strings"
)

type ProductInfo struct {
	XMLName  xml.Name `xml:"products"`
	Products []struct {
		XMLName      xml.Name `xml:"product"`
		Type         string   `xml:"type"`
		HeadFilesUrl string   `xml:"head-files-url"`
		ImageUrl     string   `xml:"image-url"`
	} `xml:"product"`
}

func (pi ProductInfo) ImageUrl(fileId []byte) *string {
	if len(pi.Products) == 0 || pi.Products[0].ImageUrl == "" {
		return nil
	} else if len(fileId) == 0 {
		return nil
	}

	fileIdHex := strings.ToLower(hex.EncodeToString(fileId))
	val := strings.Replace(pi.Products[0].ImageUrl, "{file_id}", fileIdHex, 1)
	return &val
}

// productType returns the account's product type ("premium", "free", ...)
// from the access point's ProductInfo packet, or nil while it is unknown.
// The packet arrives shortly after login, so a status read before it sees
// nil rather than a guess. The ProductInfo lives on the AppPlayer, which is
// created per session, so the value always belongs to that session's account.
func productType(pi *ProductInfo) *string {
	if pi == nil || len(pi.Products) == 0 {
		return nil
	}

	val := strings.ToLower(strings.TrimSpace(pi.Products[0].Type))
	if val == "" {
		return nil
	}
	return &val
}
