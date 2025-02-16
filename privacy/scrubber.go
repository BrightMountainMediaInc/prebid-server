package privacy

import (
	"encoding/json"
	"net"

	"github.com/prebid/prebid-server/v2/config"
	"github.com/prebid/prebid-server/v2/util/iputil"
	"github.com/prebid/prebid-server/v2/util/jsonutil"
	"github.com/prebid/prebid-server/v2/util/ptrutil"

	"github.com/prebid/openrtb/v19/openrtb2"
)

// ScrubStrategyIPV4 defines the approach to scrub PII from an IPV4 address.
type ScrubStrategyIPV4 int

const (
	// ScrubStrategyIPV4None does not remove any part of an IPV4 address.
	ScrubStrategyIPV4None ScrubStrategyIPV4 = iota

	// ScrubStrategyIPV4Subnet zeroes out the last 8 bits of an IPV4 address.
	ScrubStrategyIPV4Subnet
)

// ScrubStrategyIPV6 defines the approach to scrub PII from an IPV6 address.
type ScrubStrategyIPV6 int

const (
	// ScrubStrategyIPV6None does not remove any part of an IPV6 address.
	ScrubStrategyIPV6None ScrubStrategyIPV6 = iota

	// ScrubStrategyIPV6Subnet zeroes out the last 16 bits of an IPV6 sub net address.
	ScrubStrategyIPV6Subnet
)

// ScrubStrategyGeo defines the approach to scrub PII from geographical data.
type ScrubStrategyGeo int

const (
	// ScrubStrategyGeoNone does not remove any geographical data.
	ScrubStrategyGeoNone ScrubStrategyGeo = iota

	// ScrubStrategyGeoFull removes all geographical data.
	ScrubStrategyGeoFull

	// ScrubStrategyGeoReducedPrecision anonymizes geographical data with rounding.
	ScrubStrategyGeoReducedPrecision
)

// ScrubStrategyUser defines the approach to scrub PII from user data.
type ScrubStrategyUser int

const (
	// ScrubStrategyUserNone does not remove non-location data.
	ScrubStrategyUserNone ScrubStrategyUser = iota

	// ScrubStrategyUserIDAndDemographic removes the user's buyer id, exchange id year of birth, and gender.
	ScrubStrategyUserIDAndDemographic
)

// ScrubStrategyDeviceID defines the approach to remove hardware id and device id data.
type ScrubStrategyDeviceID int

const (
	// ScrubStrategyDeviceIDNone does not remove hardware id and device id data.
	ScrubStrategyDeviceIDNone ScrubStrategyDeviceID = iota

	// ScrubStrategyDeviceIDAll removes all hardware and device id data (ifa, mac hashes device id hashes)
	ScrubStrategyDeviceIDAll
)

// Scrubber removes PII from parts of an OpenRTB request.
type Scrubber interface {
	ScrubRequest(bidRequest *openrtb2.BidRequest, enforcement Enforcement) *openrtb2.BidRequest
	ScrubDevice(device *openrtb2.Device, id ScrubStrategyDeviceID, ipv4 ScrubStrategyIPV4, ipv6 ScrubStrategyIPV6, geo ScrubStrategyGeo) *openrtb2.Device
	ScrubUser(user *openrtb2.User, strategy ScrubStrategyUser, geo ScrubStrategyGeo) *openrtb2.User
}

type scrubber struct {
	ipV6 config.IPv6
	ipV4 config.IPv4
}

// NewScrubber returns an OpenRTB scrubber.
func NewScrubber(ipV6 config.IPv6, ipV4 config.IPv4) Scrubber {
	return scrubber{
		ipV6: ipV6,
		ipV4: ipV4,
	}
}

func (s scrubber) ScrubRequest(bidRequest *openrtb2.BidRequest, enforcement Enforcement) *openrtb2.BidRequest {
	var userExtParsed map[string]json.RawMessage
	userExtModified := false

	// expressed in two lines because IntelliJ cannot infer the generic type
	var userCopy *openrtb2.User
	userCopy = ptrutil.Clone(bidRequest.User)

	// expressed in two lines because IntelliJ cannot infer the generic type
	var deviceCopy *openrtb2.Device
	deviceCopy = ptrutil.Clone(bidRequest.Device)

	if userCopy != nil && (enforcement.UFPD || enforcement.Eids) {
		if len(userCopy.Ext) != 0 {
			jsonutil.Unmarshal(userCopy.Ext, &userExtParsed)
		}
	}

	if enforcement.UFPD {
		// transmitUfpd covers user.ext.data, user.data, user.id, user.buyeruid, user.yob, user.gender, user.keywords, user.kwarray
		// and device.{ifa, macsha1, macmd5, dpidsha1, dpidmd5, didsha1, didmd5}
		if deviceCopy != nil {
			deviceCopy.DIDMD5 = ""
			deviceCopy.DIDSHA1 = ""
			deviceCopy.DPIDMD5 = ""
			deviceCopy.DPIDSHA1 = ""
			deviceCopy.IFA = ""
			deviceCopy.MACMD5 = ""
			deviceCopy.MACSHA1 = ""
		}
		if userCopy != nil {
			userCopy.Data = nil
			userCopy.ID = ""
			userCopy.BuyerUID = ""
			userCopy.Yob = 0
			userCopy.Gender = ""
			userCopy.Keywords = ""
			userCopy.KwArray = nil

			_, hasField := userExtParsed["data"]
			if hasField {
				delete(userExtParsed, "data")
				userExtModified = true
			}
		}
	}
	if enforcement.Eids {
		//transmitEids covers user.eids and user.ext.eids
		if userCopy != nil {
			userCopy.EIDs = nil
			_, hasField := userExtParsed["eids"]
			if hasField {
				delete(userExtParsed, "eids")
				userExtModified = true
			}
		}
	}

	if userExtModified {
		userExt, _ := jsonutil.Marshal(userExtParsed)
		userCopy.Ext = userExt
	}

	if enforcement.TID {
		//remove source.tid and imp.ext.tid
		if bidRequest.Source != nil {
			sourceCopy := ptrutil.Clone(bidRequest.Source)
			sourceCopy.TID = ""
			bidRequest.Source = sourceCopy
		}
		for ind, imp := range bidRequest.Imp {
			impExt := scrubExtIDs(imp.Ext, "tid")
			bidRequest.Imp[ind].Ext = impExt
		}
	}

	if enforcement.PreciseGeo {
		//round user's geographic location by rounding off IP address and lat/lng data.
		//this applies to both device.geo and user.geo
		if userCopy != nil && userCopy.Geo != nil {
			userCopy.Geo = scrubGeoPrecision(userCopy.Geo)
		}

		if deviceCopy != nil {
			if deviceCopy.Geo != nil {
				deviceCopy.Geo = scrubGeoPrecision(deviceCopy.Geo)
			}
			deviceCopy.IP = scrubIP(deviceCopy.IP, s.ipV4.AnonKeepBits, iputil.IPv4BitSize)
			deviceCopy.IPv6 = scrubIP(deviceCopy.IPv6, s.ipV6.AnonKeepBits, iputil.IPv6BitSize)
		}
	}

	bidRequest.Device = deviceCopy
	bidRequest.User = userCopy
	return bidRequest
}

func (s scrubber) ScrubDevice(device *openrtb2.Device, id ScrubStrategyDeviceID, ipv4 ScrubStrategyIPV4, ipv6 ScrubStrategyIPV6, geo ScrubStrategyGeo) *openrtb2.Device {
	if device == nil {
		return nil
	}

	deviceCopy := *device

	switch id {
	case ScrubStrategyDeviceIDAll:
		deviceCopy.DIDMD5 = ""
		deviceCopy.DIDSHA1 = ""
		deviceCopy.DPIDMD5 = ""
		deviceCopy.DPIDSHA1 = ""
		deviceCopy.IFA = ""
		deviceCopy.MACMD5 = ""
		deviceCopy.MACSHA1 = ""
	}

	switch ipv4 {
	case ScrubStrategyIPV4Subnet:
		deviceCopy.IP = scrubIP(device.IP, s.ipV4.AnonKeepBits, iputil.IPv4BitSize)
	}

	switch ipv6 {
	case ScrubStrategyIPV6Subnet:
		deviceCopy.IPv6 = scrubIP(device.IPv6, s.ipV6.AnonKeepBits, iputil.IPv6BitSize)
	}

	switch geo {
	case ScrubStrategyGeoFull:
		deviceCopy.Geo = scrubGeoFull(device.Geo)
	case ScrubStrategyGeoReducedPrecision:
		deviceCopy.Geo = scrubGeoPrecision(device.Geo)
	}

	return &deviceCopy
}

func (scrubber) ScrubUser(user *openrtb2.User, strategy ScrubStrategyUser, geo ScrubStrategyGeo) *openrtb2.User {
	if user == nil {
		return nil
	}

	userCopy := *user

	if strategy == ScrubStrategyUserIDAndDemographic {
		userCopy.BuyerUID = ""
		userCopy.ID = ""
		userCopy.Ext = scrubExtIDs(userCopy.Ext, "eids")
		userCopy.Yob = 0
		userCopy.Gender = ""
	}

	switch geo {
	case ScrubStrategyGeoFull:
		userCopy.Geo = scrubGeoFull(user.Geo)
	case ScrubStrategyGeoReducedPrecision:
		userCopy.Geo = scrubGeoPrecision(user.Geo)
	}

	return &userCopy
}

func scrubIP(ip string, ones, bits int) string {
	if ip == "" {
		return ""
	}
	ipMask := net.CIDRMask(ones, bits)
	ipMasked := net.ParseIP(ip).Mask(ipMask)
	return ipMasked.String()
}

func scrubGeoFull(geo *openrtb2.Geo) *openrtb2.Geo {
	if geo == nil {
		return nil
	}

	return &openrtb2.Geo{}
}

func scrubGeoPrecision(geo *openrtb2.Geo) *openrtb2.Geo {
	if geo == nil {
		return nil
	}

	geoCopy := *geo
	geoCopy.Lat = float64(int(geo.Lat*100.0+0.5)) / 100.0 // Round Latitude
	geoCopy.Lon = float64(int(geo.Lon*100.0+0.5)) / 100.0 // Round Longitude
	return &geoCopy
}

func scrubExtIDs(ext json.RawMessage, fieldName string) json.RawMessage {
	if len(ext) == 0 {
		return ext
	}

	var userExtParsed map[string]json.RawMessage
	err := jsonutil.Unmarshal(ext, &userExtParsed)
	if err != nil {
		return ext
	}

	_, hasField := userExtParsed[fieldName]
	if hasField {
		delete(userExtParsed, fieldName)
		result, err := jsonutil.Marshal(userExtParsed)
		if err == nil {
			return result
		}
	}

	return ext
}
