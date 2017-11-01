package appnexus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/prebid/prebid-server/pbs"

	"golang.org/x/net/context/ctxhttp"

	"github.com/mxmCherry/openrtb"
	"github.com/prebid/prebid-server/adapters"
)

type XhbAdapter struct {
	http         *adapters.HTTPAdapter
	URI          string
	usersyncInfo *pbs.UsersyncInfo
}

/* Name - export adapter name */
func (a *XhbAdapter) Name() string {
	return "xhb"
}

// used for cookies and such
func (a *XhbAdapter) FamilyName() string {
	return "xhb"
}

func (a *XhbAdapter) GetUsersyncInfo() *pbs.UsersyncInfo {
	return a.usersyncInfo
}

func (a *XhbAdapter) SkipNoCookies() bool {
	return false
}

type KeyVal struct {
	Key    string   `json:"key,omitempty"`
	Values []string `json:"value,omitempty"`
}

type appnexusParams struct {
	PlacementId       int      `json:"placementId"`
	InvCode           string   `json:"invCode"`
	Member            string   `json:"member"`
	Keywords          []KeyVal `json:"keywords"`
	TrafficSourceCode string   `json:"trafficSourceCode"`
	Reserve           float64  `json:"reserve"`
	Position          string   `json:"position"`
}

type appnexusImpExtAppnexus struct {
	PlacementID       int    `json:"placement_id,omitempty"`
	Keywords          string `json:"keywords,omitempty"`
	TrafficSourceCode string `json:"traffic_source_code,omitempty"`
}

type appnexusImpExt struct {
	Appnexus appnexusImpExtAppnexus `json:"appnexus"`
}

func (a *XhbAdapter) Call(ctx context.Context, req *pbs.PBSRequest, bidder *pbs.PBSBidder) (pbs.PBSBidSlice, error) {
	supportedMediaTypes := []pbs.MediaType{pbs.MEDIA_TYPE_BANNER, pbs.MEDIA_TYPE_VIDEO}
	anReq, err := adapters.MakeOpenRTBGeneric(req, bidder, a.FamilyName(), supportedMediaTypes, true)

	if err != nil {
		return nil, err
	}
	uri := a.URI
	for i, unit := range bidder.AdUnits {
		var params appnexusParams
		err := json.Unmarshal(unit.Params, &params)
		if err != nil {
			return nil, err
		}

		if params.PlacementId == 0 && (params.InvCode == "" || params.Member == "") {
			return nil, errors.New("No placement or member+invcode provided")
		}

		if params.InvCode != "" {
			anReq.Imp[i].TagID = params.InvCode
			if params.Member != "" {
				// this assumes that the same member ID is used across all tags, which should be the case
				uri = fmt.Sprintf("%s?member_id=%s", a.URI, params.Member)
			}

		}
		if params.Reserve > 0 {
			anReq.Imp[i].BidFloor = params.Reserve // TODO: we need to factor in currency here if non-USD
		}
		if anReq.Imp[i].Banner != nil && params.Position != "" {
			if params.Position == "above" {
				anReq.Imp[i].Banner.Pos = openrtb.AdPositionAboveTheFold.Ptr()
			} else if params.Position == "below" {
				anReq.Imp[i].Banner.Pos = openrtb.AdPositionBelowTheFold.Ptr()
			}
		}

		kvs := make([]string, 0, len(params.Keywords)*2)
		for _, kv := range params.Keywords {
			if len(kv.Values) == 0 {
				kvs = append(kvs, kv.Key)
			} else {
				for _, val := range kv.Values {
					kvs = append(kvs, fmt.Sprintf("%s=%s", kv.Key, val))
				}

			}
		}

		keywordStr := strings.Join(kvs, ",")

		impExt := appnexusImpExt{Appnexus: appnexusImpExtAppnexus{
			PlacementID:       params.PlacementId,
			TrafficSourceCode: params.TrafficSourceCode,
			Keywords:          keywordStr,
		}}
		anReq.Imp[i].Ext, err = json.Marshal(&impExt)
	}

	reqJSON, err := json.Marshal(anReq)
	if err != nil {
		return nil, err
	}

	debug := &pbs.BidderDebug{
		RequestURI: uri,
	}

	if req.IsDebug {
		debug.RequestBody = string(reqJSON)
		bidder.Debug = append(bidder.Debug, debug)
	}

	httpReq, err := http.NewRequest("POST", uri, bytes.NewBuffer(reqJSON))
	httpReq.Header.Add("Content-Type", "application/json;charset=utf-8")
	httpReq.Header.Add("Accept", "application/json")

	anResp, err := ctxhttp.Do(ctx, a.http.Client, httpReq)
	if err != nil {
		return nil, err
	}

	debug.StatusCode = anResp.StatusCode

	if anResp.StatusCode == 204 {
		return nil, nil
	}

	defer anResp.Body.Close()
	body, err := ioutil.ReadAll(anResp.Body)
	if err != nil {
		return nil, err
	}
	responseBody := string(body)

	if anResp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP status %d; body: %s", anResp.StatusCode, responseBody)
	}

	if req.IsDebug {
		debug.ResponseBody = responseBody
	}

	var bidResp openrtb.BidResponse
	err = json.Unmarshal(body, &bidResp)
	if err != nil {
		return nil, err
	}

	bids := make(pbs.PBSBidSlice, 0)

	numBids := 0
	for _, sb := range bidResp.SeatBid {
		for _, bid := range sb.Bid {
			numBids++

			bidID := bidder.LookupBidID(bid.ImpID)
			if bidID == "" {
				return nil, fmt.Errorf("Unknown ad unit code '%s'", bid.ImpID)
			}

			pbid := pbs.PBSBid{
				BidID:       bidID,
				AdUnitCode:  bid.ImpID,
				BidderCode:  bidder.BidderCode,
				Price:       0.01,
				Adm:         bid.AdM,
				Creative_id: bid.CrID,
				Width:       bid.W,
				Height:      bid.H,
				DealId:      99999999,
				NURL:        bid.NURL,
			}

			mediaType := getMediaTypeForImp(bid.ImpID, anReq.Imp)
			pbid.CreativeMediaType = mediaType
			bids = append(bids, &pbid)
		}
	}

	return bids, nil
}
func getMediaTypeForImp(impId string, imps []openrtb.Imp) string {
	mediaType := "banner"
	for _, imp := range imps {
		if imp.ID == impId {
			if imp.Video != nil {
				mediaType = "video"
			}
			return mediaType
		}
	}
	return mediaType
}

func NewXhbAdapter(config *adapters.HTTPAdapterConfig, externalURL string) *XhbAdapter {
	a := adapters.NewHTTPAdapter(config)

	redirect_uri := fmt.Sprintf("%s/setuid?bidder=adnxs&uid=$UID", externalURL)
	usersyncURL := "//ib.adnxs.com/getuid?"

	info := &pbs.UsersyncInfo{
		URL:         fmt.Sprintf("%s%s", usersyncURL, url.QueryEscape(redirect_uri)),
		Type:        "redirect",
		SupportCORS: false,
	}

	return &XhbAdapter{
		http:         a,
		URI:          "http://ib.adnxs.com/openrtb2",
		usersyncInfo: info,
	}
}