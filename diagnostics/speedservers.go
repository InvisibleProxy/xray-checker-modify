package diagnostics

import (
	"net/url"
	"strings"
)

// CapabilitySpeedServersV1 is advertised by an agent that can download from a
// catalogue server named by ID. An agent without it measures its own configured
// download URL, and its rate is then not comparable with the run's.
const CapabilitySpeedServersV1 = "speed-servers-v1"

// SpeedServer is a public speed-test endpoint that the controller and the agent
// both know by ID.
//
// It exists because a rate only means something next to another rate taken from
// the same server. The checker measures each node against a test URL of its
// own — a per-node override, a country reserve, the global default — while an
// agent used to download whatever its own configuration named. A node measured
// from Moscow against a New York server and from Amsterdam against a French one
// yields two numbers that differ for reasons that have nothing to do with the
// node, and the verdict built on them was read as evidence about the node.
//
// The catalogue is compiled into both binaries on purpose. A job still names an
// ID and never a URL, so the controller cannot turn a diagnostic job into an
// arbitrary fetch from the agent's network; it can only ask for one of these.
type SpeedServer struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	City        string `json:"city"`
	CountryCode string `json:"countryCode"`
	// URL is the file the agent downloads. A request may stop short of its end:
	// the job's transfer size decides how much of it is read.
	URL string `json:"url"`
}

// The entries mirror country-test-urls.example.yaml, keeping its endpoint IDs, so
// a reserve URL the checker used resolves to the same name here. The list is
// matched by host, which is what identifies the server: the file on it only
// sets how much can be read.
var speedServerCatalogue = []SpeedServer{
	{ID: "hetzner-falkenstein-fsn1", Provider: "Hetzner", City: "Falkenstein", CountryCode: "DE", URL: "https://fsn1-speed.hetzner.com/100MB.bin"},
	{ID: "hostkey-frankfurt", Provider: "HOSTKEY", City: "Frankfurt", CountryCode: "DE", URL: "https://spd-desrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-frankfurt", Provider: "EDIS Global", City: "Frankfurt", CountryCode: "DE", URL: "https://de.edisglobal.com/100MB.test"},
	{ID: "leaseweb-frankfurt-fra1", Provider: "Leaseweb", City: "Frankfurt", CountryCode: "DE", URL: "http://speedtest.fra1.de.leaseweb.net/100mb.bin"},
	{ID: "hostkey-amsterdam", Provider: "HOSTKEY", City: "Amsterdam", CountryCode: "NL", URL: "https://spd-nlsrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-amsterdam", Provider: "EDIS Global", City: "Amsterdam", CountryCode: "NL", URL: "https://nl.edisglobal.com/100MB.test"},
	{ID: "leaseweb-amsterdam-ams1", Provider: "Leaseweb", City: "Amsterdam AMS-01", CountryCode: "NL", URL: "http://speedtest.ams1.nl.leaseweb.net/100mb.bin"},
	{ID: "leaseweb-amsterdam-ams2", Provider: "Leaseweb", City: "Amsterdam AMS-02", CountryCode: "NL", URL: "http://speedtest.ams2.nl.leaseweb.net/100mb.bin"},
	{ID: "edis-tallinn", Provider: "EDIS Global", City: "Tallinn", CountryCode: "EE", URL: "https://ee.edisglobal.com/100MB.test"},
	{ID: "fairyhosting-estonia", Provider: "FairyHosting", City: "Estonia", CountryCode: "EE", URL: "https://lg.fairyhosting.com/100MB.test"},
	{ID: "hostkey-helsinki", Provider: "HOSTKEY", City: "Helsinki", CountryCode: "FI", URL: "https://spd-fisrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-helsinki", Provider: "EDIS Global", City: "Helsinki", CountryCode: "FI", URL: "https://fi.edisglobal.com/100MB.test"},
	{ID: "mynymbox-helsinki", Provider: "MyNymBox", City: "Helsinki", CountryCode: "FI", URL: "https://fi-lg.mynymbox.io/100MB.bin"},
	{ID: "hetzner-helsinki-hel1", Provider: "Hetzner", City: "Helsinki HEL1", CountryCode: "FI", URL: "https://hel1-speed.hetzner.com/100MB.bin"},
	{ID: "hostkey-new-york", Provider: "HOSTKEY", City: "New York", CountryCode: "US", URL: "https://spd-uswb.hostkey.com/files/1000mb.bin"},
	{ID: "edis-new-york", Provider: "EDIS Global", City: "New York", CountryCode: "US", URL: "https://us.edisglobal.com/100MB.test"},
	{ID: "leaseweb-new-york-nyc1", Provider: "Leaseweb", City: "New York NYC-01", CountryCode: "US", URL: "http://speedtest.nyc1.us.leaseweb.net/100mb.bin"},
	{ID: "leaseweb-washington-wdc2", Provider: "Leaseweb", City: "Washington WDC-02", CountryCode: "US", URL: "http://speedtest.wdc2.us.leaseweb.net/100mb.bin"},
	{ID: "edis-los-angeles", Provider: "EDIS Global", City: "Los Angeles", CountryCode: "US", URL: "https://uslax.edisglobal.com/100MB.test"},
	{ID: "leaseweb-los-angeles-lax12", Provider: "Leaseweb", City: "Los Angeles LAX-12", CountryCode: "US", URL: "http://speedtest.lax12.us.leaseweb.net/100mb.bin"},
	{ID: "edis-miami", Provider: "EDIS Global", City: "Miami", CountryCode: "US", URL: "https://usmia.edisglobal.com/100MB.test"},
	{ID: "hostkey-london", Provider: "HOSTKEY", City: "London", CountryCode: "GB", URL: "http://spd-uksrv.hostkey.com/files/100mb.bin"},
	{ID: "leaseweb-london-lon1", Provider: "Leaseweb", City: "London LON-01", CountryCode: "GB", URL: "http://speedtest.lon1.uk.leaseweb.net/100mb.bin"},
	{ID: "edis-london", Provider: "EDIS Global", City: "London", CountryCode: "GB", URL: "https://uk.edisglobal.com/100MB.test"},
	{ID: "hostkey-reykjavik", Provider: "HOSTKEY", City: "Reykjavik", CountryCode: "IS", URL: "https://spd-icsrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-hafnarfjordur", Provider: "EDIS Global", City: "Hafnarfjordur", CountryCode: "IS", URL: "https://is.edisglobal.com/100MB.test"},
	{ID: "hostkey-paris", Provider: "HOSTKEY", City: "Paris", CountryCode: "FR", URL: "https://spd-frsrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-paris", Provider: "EDIS Global", City: "Paris", CountryCode: "FR", URL: "https://fr.edisglobal.com/100MB.test"},
	// proof.ovh.net is the agent's own default download URL and the checker's
	// default speed-test URL, so it has to be nameable as well: a run that used
	// the default is otherwise the one comparison that can never be made.
	{ID: "ovh-proof", Provider: "OVHcloud", City: "Roubaix", CountryCode: "FR", URL: "https://proof.ovh.net/files/100Mb.dat"},
	{ID: "hostkey-madrid", Provider: "HOSTKEY", City: "Madrid", CountryCode: "ES", URL: "https://spd-essrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-seville", Provider: "EDIS Global", City: "Seville", CountryCode: "ES", URL: "https://es.edisglobal.com/100MB.test"},
	{ID: "hostkey-milan", Provider: "HOSTKEY", City: "Milan", CountryCode: "IT", URL: "https://spd-itsrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-milan", Provider: "EDIS Global", City: "Milan", CountryCode: "IT", URL: "https://it.edisglobal.com/100MB.test"},
	{ID: "hostkey-istanbul", Provider: "HOSTKEY", City: "Istanbul", CountryCode: "TR", URL: "http://spd-tr.hostkey.com/files/100mb.bin"},
	{ID: "hostkey-warsaw", Provider: "HOSTKEY", City: "Warsaw", CountryCode: "PL", URL: "http://spd-plsrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-warsaw", Provider: "EDIS Global", City: "Warsaw", CountryCode: "PL", URL: "https://pl.edisglobal.com/100MB.test"},
	{ID: "hostkey-zurich", Provider: "HOSTKEY", City: "Zurich", CountryCode: "CH", URL: "http://spd-chsrv.hostkey.com/files/100mb.bin"},
	{ID: "edis-zurich", Provider: "EDIS Global", City: "Zurich", CountryCode: "CH", URL: "https://ch.edisglobal.com/100MB.test"},
}

// SpeedServers returns the catalogue in presentation order.
func SpeedServers() []SpeedServer {
	return append([]SpeedServer(nil), speedServerCatalogue...)
}

// SpeedServerByID resolves an ID from a job. An unknown ID is not an error the
// agent can do anything about; it measures its own URL and says so by leaving
// the observation's server empty.
func SpeedServerByID(id string) (SpeedServer, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return SpeedServer{}, false
	}
	for _, server := range speedServerCatalogue {
		if server.ID == id {
			return server, true
		}
	}
	return SpeedServer{}, false
}

// SpeedServerForURL names the catalogue server a test URL points at. Scheme,
// port, path and query are ignored: two files on one host come through the same
// uplink, which is the thing being compared.
func SpeedServerForURL(rawURL string) (SpeedServer, bool) {
	host := speedServerHost(rawURL)
	if host == "" {
		return SpeedServer{}, false
	}
	for _, server := range speedServerCatalogue {
		if speedServerHost(server.URL) == host {
			return server, true
		}
	}
	return SpeedServer{}, false
}

func speedServerHost(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
}

func validSpeedServerID(id string) bool {
	_, ok := SpeedServerByID(id)
	return ok
}
