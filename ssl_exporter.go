package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "ssl"
)

var (
	httpsConnectSuccess = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "https_connect_success"),
		"If the TLS connection was a success",
		nil, nil,
	)
	notBefore = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_not_before"),
		"NotBefore expressed as a Unix Epoch Time",
		[]string{"serial_no", "issuer_cn"}, nil,
	)
	notAfter = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_not_after"),
		"NotAfter expressed as a Unix Epoch Time",
		[]string{"serial_no", "issuer_cn"}, nil,
	)
	commonName = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_subject_common_name"),
		"Subject Common Name",
		[]string{"serial_no", "issuer_cn", "subject_cn"}, nil,
	)
	subjectAlernativeDNSNames = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_subject_alternative_dnsnames"),
		"Subject Alternative DNS Names",
		[]string{"serial_no", "issuer_cn", "dnsnames"}, nil,
	)
	subjectAlernativeIPs = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_subject_alternative_ips"),
		"Subject Alternative IPs",
		[]string{"serial_no", "issuer_cn", "ips"}, nil,
	)
	subjectAlernativeEmailAddresses = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_subject_alternative_emails"),
		"Subject Alternative Email Addresses",
		[]string{"serial_no", "issuer_cn", "emails"}, nil,
	)
	subjectOrganizationUnits = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "cert_subject_organization_units"),
		"Subject Organization Units",
		[]string{"serial_no", "issuer_cn", "subject_ou"}, nil,
	)
)

// Exporter is the exporter type...
type Exporter struct {
	target    string
	timeout   time.Duration
	tlsConfig *tls.Config
}

// Describe metrics
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- httpsConnectSuccess
	ch <- notAfter
	ch <- commonName
	ch <- subjectAlernativeDNSNames
	ch <- subjectAlernativeIPs
	ch <- subjectAlernativeEmailAddresses
	ch <- subjectOrganizationUnits
}

// Collect metrics
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {

	// Create the HTTP client and make a get request of the target
	tr := &http.Transport{
		TLSClientConfig: e.tlsConfig,
	}
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: tr,
		Timeout:   e.timeout,
	}
	resp, err := client.Get(e.target)

	if err != nil {
		log.Errorln(err)
		ch <- prometheus.MustNewConstMetric(
			httpsConnectSuccess, prometheus.GaugeValue, 0,
		)
		return
	}

	if resp.TLS == nil {
		log.Errorln("The response from " + e.target + " is unencrypted")
		ch <- prometheus.MustNewConstMetric(
			httpsConnectSuccess, prometheus.GaugeValue, 0,
		)
		return
	}

	ch <- prometheus.MustNewConstMetric(
		httpsConnectSuccess, prometheus.GaugeValue, 1,
	)

	// Remove duplicate certificates from the response
	peerCertificates := uniq(resp.TLS.PeerCertificates)

	// Loop through returned certificates and create metrics
	for _, cert := range peerCertificates {

		subjectCN := cert.Subject.CommonName
		issuerCN := cert.Issuer.CommonName
		subjectDNSNames := cert.DNSNames
		subjectEmails := cert.EmailAddresses
		subjectIPs := cert.IPAddresses
		serialNum := cert.SerialNumber.String()
		subjectOUs := cert.Subject.OrganizationalUnit

		if !cert.NotAfter.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				notAfter, prometheus.GaugeValue, float64(cert.NotAfter.UnixNano()/1e9), serialNum, issuerCN,
			)
		}

		if !cert.NotBefore.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				notBefore, prometheus.GaugeValue, float64(cert.NotBefore.UnixNano()/1e9), serialNum, issuerCN,
			)
		}

		if subjectCN != "" {
			ch <- prometheus.MustNewConstMetric(
				commonName, prometheus.GaugeValue, 1, serialNum, issuerCN, subjectCN,
			)
		}

		if len(subjectDNSNames) > 0 {
			ch <- prometheus.MustNewConstMetric(
				subjectAlernativeDNSNames, prometheus.GaugeValue, 1, serialNum, issuerCN, ","+strings.Join(subjectDNSNames, ",")+",",
			)
		}

		if len(subjectEmails) > 0 {
			ch <- prometheus.MustNewConstMetric(
				subjectAlernativeEmailAddresses, prometheus.GaugeValue, 1, serialNum, issuerCN, ","+strings.Join(subjectEmails, ",")+",",
			)
		}

		if len(subjectIPs) > 0 {
			i := ","
			for _, ip := range subjectIPs {
				i = i + ip.String() + ","
			}
			ch <- prometheus.MustNewConstMetric(
				subjectAlernativeIPs, prometheus.GaugeValue, 1, serialNum, issuerCN, i,
			)
		}

		if len(subjectIPs) > 0 {
			ch <- prometheus.MustNewConstMetric(
				subjectOrganizationUnits, prometheus.GaugeValue, 1, serialNum, issuerCN, ","+strings.Join(subjectOUs, ",")+",",
			)
		}
	}
}

func probeHandler(w http.ResponseWriter, r *http.Request, tlsConfig *tls.Config) {

	target := r.URL.Query().Get("target")

	// The following timeout block was taken wholly from the blackbox exporter
	//   https://github.com/prometheus/blackbox_exporter/blob/master/main.go
	var timeoutSeconds float64
	if v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"); v != "" {
		var err error
		timeoutSeconds, err = strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse timeout from Prometheus header: %s", err), http.StatusInternalServerError)
			return
		}
	} else {
		timeoutSeconds = 10
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = 10
	}

	timeout := time.Duration((timeoutSeconds) * 1e9)

	exporter := &Exporter{
		target:    target,
		timeout:   timeout,
		tlsConfig: tlsConfig,
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(exporter)

	// Serve
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func uniq(certs []*x509.Certificate) []*x509.Certificate {
	r := []*x509.Certificate{}

	for _, c := range certs {
		if !contains(r, c) {
			r = append(r, c)
		}
	}

	return r
}

func contains(certs []*x509.Certificate, cert *x509.Certificate) bool {
	for _, c := range certs {
		if (c.SerialNumber.String() == cert.SerialNumber.String()) && (c.Issuer.CommonName == cert.Issuer.CommonName) {
			return true
		}
	}
	return false
}

func init() {
	prometheus.MustRegister(version.NewCollector(namespace + "_exporter"))
}

func main() {
	var (
		tlsConfig     *tls.Config
		certificates  []tls.Certificate
		rootCAs       *x509.CertPool
		listenAddress = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9219").String()
		metricsPath   = kingpin.Flag("web.metrics-path", "Path under which to expose metrics").Default("/metrics").String()
		probePath     = kingpin.Flag("web.probe-path", "Path under which to expose the probe endpoint").Default("/probe").String()
		insecure      = kingpin.Flag("tls.insecure", "Skip certificate verification").Default("false").Bool()
		clientAuth    = kingpin.Flag("tls.client-auth", "Enable client authentication").Default("false").Bool()
		caFile        = kingpin.Flag("tls.cacert", "Local path to an alternative CA cert bundle").String()
		certFile      = kingpin.Flag("tls.cert", "Local path to a client certificate file (for client authentication)").String()
		keyFile       = kingpin.Flag("tls.key", "Local path to a private key file (for client authentication)").String()
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print(namespace + "_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	if *clientAuth {

		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Fatalln(err)
		}
		certificates = append(certificates, cert)

		caCert, err := ioutil.ReadFile(*caFile)
		if err != nil {
			log.Fatalln(err)
		}

		rootCAs = x509.NewCertPool()
		rootCAs.AppendCertsFromPEM(caCert)
	}

	tlsConfig = &tls.Config{
		InsecureSkipVerify: *insecure,
		Certificates:       certificates,
		RootCAs:            rootCAs,
	}

	log.Infoln("Starting "+namespace+"_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	http.Handle(*metricsPath, prometheus.Handler())
	http.HandleFunc(*probePath, func(w http.ResponseWriter, r *http.Request) {
		probeHandler(w, r, tlsConfig)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
						 <head><title>SSL Exporter</title></head>
						 <body>
						 <h1>SSL Exporter</h1>
						 <p><a href="` + *probePath + `?target=https://example.com">Probe https://example.com for SSL cert metrics</a></p>
						 <p><a href='` + *metricsPath + `'>Metrics</a></p>
						 </body>
						 </html>`))
	})

	log.Infoln("Listening on", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
