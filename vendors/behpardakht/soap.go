package behpardakht

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The SOAP half. Behpardakht is an IPG, and it speaks
// SOAP rather than REST, which is why this service exists at all: two REST
// fakes would both be CPG and both spend the INSTALLMENT wallet, so neither
// would exercise the cash path.
//
// node-soap fetches the WSDL, builds a client from it, and calls
// `client.bpPayRequest(fields, cb)`. It reads the answer's `return` child, so
// every response element carries one.

const soapNS = "http://interfaces.core.sw.bps.com/"

// wsdl is served at both requestUrl and verifyUrl. The real gateway puts every
// operation on one service; the driver keeps two URLs in config, so both are
// answered with the same document rather than splitting the operations and
// making the two endpoints subtly different.
const wsdlTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<definitions name="PaymentGateway"
    targetNamespace="http://interfaces.core.sw.bps.com/"
    xmlns="http://schemas.xmlsoap.org/wsdl/"
    xmlns:soap="http://schemas.xmlsoap.org/wsdl/soap/"
    xmlns:xsd="http://www.w3.org/2001/XMLSchema"
    xmlns:tns="http://interfaces.core.sw.bps.com/">
  <types>
    <xsd:schema targetNamespace="http://interfaces.core.sw.bps.com/"
        elementFormDefault="unqualified">
      <xsd:element name="bpPayRequest">
        <xsd:complexType><xsd:sequence>
          <xsd:element name="terminalId" type="xsd:long" minOccurs="0"/>
          <xsd:element name="userName" type="xsd:string" minOccurs="0"/>
          <xsd:element name="userPassword" type="xsd:string" minOccurs="0"/>
          <xsd:element name="orderId" type="xsd:long" minOccurs="0"/>
          <xsd:element name="amount" type="xsd:long" minOccurs="0"/>
          <xsd:element name="localDate" type="xsd:string" minOccurs="0"/>
          <xsd:element name="localTime" type="xsd:string" minOccurs="0"/>
          <xsd:element name="additionalData" type="xsd:string" minOccurs="0"/>
          <xsd:element name="callBackUrl" type="xsd:string" minOccurs="0"/>
          <xsd:element name="payerId" type="xsd:long" minOccurs="0"/>
        </xsd:sequence></xsd:complexType>
      </xsd:element>
      <xsd:element name="bpPayRequestResponse">
        <xsd:complexType><xsd:sequence>
          <xsd:element name="return" type="xsd:string" minOccurs="0"/>
        </xsd:sequence></xsd:complexType>
      </xsd:element>
%s
    </xsd:schema>
  </types>
  <message name="bpPayRequestIn"><part name="parameters" element="tns:bpPayRequest"/></message>
  <message name="bpPayRequestOut"><part name="parameters" element="tns:bpPayRequestResponse"/></message>
%s
  <portType name="PaymentIF">
    <operation name="bpPayRequest">
      <input message="tns:bpPayRequestIn"/>
      <output message="tns:bpPayRequestOut"/>
    </operation>
%s
  </portType>
  <binding name="PaymentIFBinding" type="tns:PaymentIF">
    <soap:binding style="document" transport="http://schemas.xmlsoap.org/soap/http"/>
    <operation name="bpPayRequest">
      <soap:operation soapAction=""/>
      <input><soap:body use="literal"/></input>
      <output><soap:body use="literal"/></output>
    </operation>
%s
  </binding>
  <service name="PaymentGateway">
    <port name="PaymentIFPort" binding="tns:PaymentIFBinding">
      <soap:address location="%s"/>
    </port>
  </service>
</definitions>`

// The three verify-side operations share one request shape, so they are
// generated rather than written out three times.
var verifyOps = []string{"bpVerifyRequest", "bpSettleRequest", "bpReversalRequest"}

func wsdl(endpoint string) string {
	var types, messages, ports, bindings strings.Builder
	for _, op := range verifyOps {
		fmt.Fprintf(&types, `      <xsd:element name="%s">
        <xsd:complexType><xsd:sequence>
          <xsd:element name="terminalId" type="xsd:long" minOccurs="0"/>
          <xsd:element name="userName" type="xsd:string" minOccurs="0"/>
          <xsd:element name="userPassword" type="xsd:string" minOccurs="0"/>
          <xsd:element name="orderId" type="xsd:long" minOccurs="0"/>
          <xsd:element name="saleOrderId" type="xsd:long" minOccurs="0"/>
          <xsd:element name="saleReferenceId" type="xsd:long" minOccurs="0"/>
        </xsd:sequence></xsd:complexType>
      </xsd:element>
      <xsd:element name="%sResponse">
        <xsd:complexType><xsd:sequence>
          <xsd:element name="return" type="xsd:string" minOccurs="0"/>
        </xsd:sequence></xsd:complexType>
      </xsd:element>
`, op, op)
		fmt.Fprintf(&messages, `  <message name="%sIn"><part name="parameters" element="tns:%s"/></message>
  <message name="%sOut"><part name="parameters" element="tns:%sResponse"/></message>
`, op, op, op, op)
		fmt.Fprintf(&ports, `    <operation name="%s">
      <input message="tns:%sIn"/>
      <output message="tns:%sOut"/>
    </operation>
`, op, op, op)
		fmt.Fprintf(&bindings, `    <operation name="%s">
      <soap:operation soapAction=""/>
      <input><soap:body use="literal"/></input>
      <output><soap:body use="literal"/></output>
    </operation>
`, op)
	}
	return fmt.Sprintf(wsdlTemplate, types.String(), messages.String(),
		ports.String(), bindings.String(), endpoint)
}

// payRequest is the body of bpPayRequest, decoded without caring about
// namespaces: encoding/xml matches on local name when none is given.
type payRequest struct {
	TerminalID     int64  `xml:"terminalId"`
	UserName       string `xml:"userName"`
	UserPassword   string `xml:"userPassword"`
	OrderID        int64  `xml:"orderId"`
	Amount         int64  `xml:"amount"`
	LocalDate      string `xml:"localDate"`
	LocalTime      string `xml:"localTime"`
	AdditionalData string `xml:"additionalData"`
	CallBackURL    string `xml:"callBackUrl"`
	PayerID        int64  `xml:"payerId"`
}

type verifyRequest struct {
	TerminalID      int64  `xml:"terminalId"`
	UserName        string `xml:"userName"`
	UserPassword    string `xml:"userPassword"`
	OrderID         int64  `xml:"orderId"`
	SaleOrderID     int64  `xml:"saleOrderId"`
	SaleReferenceID int64  `xml:"saleReferenceId"`
}

type soapEnvelope struct {
	Pay     *payRequest    `xml:"Body>bpPayRequest"`
	Verify  *verifyRequest `xml:"Body>bpVerifyRequest"`
	Settle  *verifyRequest `xml:"Body>bpSettleRequest"`
	Reverse *verifyRequest `xml:"Body>bpReversalRequest"`
}

func writeSOAP(w http.ResponseWriter, operation, value string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body>
    <ns2:%sResponse xmlns:ns2="%s">
      <return>%s</return>
    </ns2:%sResponse>
  </soap:Body>
</soap:Envelope>`, operation, soapNS, xmlEscape(value), operation)
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func readEnvelope(r *http.Request) (*soapEnvelope, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var env soapEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	return &env, nil
}
