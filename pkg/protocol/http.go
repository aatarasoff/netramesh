package protocol

import (
	"bufio"
	"bytes"
	"container/list"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/opentracing/opentracing-go"
	"github.com/patrickmn/go-cache"
	"github.com/uber/jaeger-client-go"

	"github.com/Lookyan/netramesh/internal/config"
	nhttp "github.com/Lookyan/netramesh/pkg/http"
	"github.com/Lookyan/netramesh/pkg/log"
)

var dumbReader = bytes.NewReader([]byte{})
var readerPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewReaderSize(dumbReader, 0xfff)
	},
}

var dumbWriter = bytes.NewBuffer([]byte{})
var writerPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(dumbWriter, 0xfff)
	},
}

// HTTPHandler process HTTP protocol
type HTTPHandler struct {
	tracingContextMapping     *cache.Cache
	routingInfoContextMapping *cache.Cache
	logger                    *log.Logger
}

// NewHTTPHandler returns HTTP handler
func NewHTTPHandler(
	logger *log.Logger,
	tracingContextMapping *cache.Cache,
	routingInfoContextMapping *cache.Cache) *HTTPHandler {
	return &HTTPHandler{
		tracingContextMapping:     tracingContextMapping,
		routingInfoContextMapping: routingInfoContextMapping,
		logger:                    logger,
	}
}

// HandleRequest handles HTTP request
func (h *HTTPHandler) HandleRequest(
	r *net.TCPConn,
	w *net.TCPConn,
	connCh chan *net.TCPConn,
	addrCh chan string,
	netRequest NetRequest,
	isInboundConn bool,
	originalDst string) *net.TCPConn {

	netHTTPRequest := netRequest.(*NetHTTPRequest)
	tmpWriter := NewTempWriter()
	defer tmpWriter.Close()
	readerWithFallback := io.TeeReader(r, tmpWriter)
	bufioHTTPReader := readerPool.Get().(*bufio.Reader)
	bufioHTTPReader.Reset(readerWithFallback)
	defer readerPool.Put(bufioHTTPReader)
	if config.GetHTTPConfig().RoutingEnabled {
		defer func() {
			if addrCh != nil {
				close(addrCh)
			}
		}()
	}
	for {
		tmpWriter.Start()
		req, err := nhttp.ReadRequest(bufioHTTPReader)
		if err == io.EOF {
			h.logger.Debug("EOF while parsing request HTTP")
			return w
		}
		if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
			h.logger.Debug(err.Error())
			return w
		}

		if req != nil {
			if req.Header.Get(config.GetHTTPConfig().RequestIdHeaderName) == "" {
				req.Header.Set(config.GetHTTPConfig().RequestIdHeaderName, uuid.New().String())
			}

			if config.GetHTTPConfig().RoutingEnabled {
				// check Cookie if enabled
				currentRoutingHeaderValue := ""
				if config.GetHTTPConfig().RoutingCookieEnabled {
					cookie, err := req.Cookie(config.GetHTTPConfig().RoutingCookieName)
					if err == nil {
						currentRoutingHeaderValue = cookie.Value
					}
				}
				if currentRoutingHeaderValue == "" {
					currentRoutingHeaderValue = req.Header.Get(config.GetHTTPConfig().RoutingHeaderName)
				}
				if currentRoutingHeaderValue == "" {
					routingContext, ok := h.routingInfoContextMapping.Get(
						req.Header.Get(config.GetHTTPConfig().RequestIdHeaderName),
					)
					if ok {
						currentRoutingHeaderValue = routingContext.(string)
						req.Header.Add(config.GetHTTPConfig().RoutingHeaderName, currentRoutingHeaderValue)
					}
				}

				// here we can override destination (DNS allowed)
				if currentRoutingHeaderValue != "" {
					addr, err := getRoutingDestination(currentRoutingHeaderValue, req.Host, originalDst)
					if err != nil {
						log.Warning(err.Error())
						addrCh <- originalDst
					} else {
						if isInboundConn {
							if rID := req.Header.Get(config.GetHTTPConfig().RequestIdHeaderName); rID != "" {
								h.routingInfoContextMapping.SetDefault(
									rID,
									currentRoutingHeaderValue,
								)
							}
							addrCh <- originalDst
						} else {
							addrCh <- addr
						}
					}
				} else {
					addrCh <- originalDst
				}

				w = <-connCh
				if w == nil {
					return w
				}
			}
		}

		if w == nil {
			return nil
		}

		if isInboundConn {
			netHTTPRequest.remoteAddr = r.RemoteAddr().String()
		} else {
			if w != nil {
				netHTTPRequest.remoteAddr = w.RemoteAddr().String()
			}
		}
		if err != nil {
			h.logger.Warningf("Error while parsing http request '%s'", err.Error())
			buf := bufferPool.Get().([]byte)
			_, err = io.CopyBuffer(w, tmpWriter, buf)
			bufferPool.Put(buf)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			tmpWriter.Stop()
			buf = bufferPool.Get().([]byte)
			_, err = io.CopyBuffer(w, bufioHTTPReader, buf)
			bufferPool.Put(buf)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			return w
		}
		// avoid ws connections and other upgrade protos
		if strings.ToLower(req.Header.Get("Connection")) == "upgrade" {
			buf := bufferPool.Get().([]byte)
			_, err = io.CopyBuffer(w, tmpWriter, buf)
			bufferPool.Put(buf)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			tmpWriter.Stop()
			buf = bufferPool.Get().([]byte)
			_, err = io.CopyBuffer(w, bufioHTTPReader, buf)
			bufferPool.Put(buf)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			return w
		}

		tmpWriter.Stop()

		if !isInboundConn {
			// we need to generate context header and propagate it
			tracingInfoByRequestID, ok := h.tracingContextMapping.Get(
				req.Header.Get(config.GetHTTPConfig().RequestIdHeaderName),
			)
			if ok {
				//h.logger.Debugf("Found request-id matching: %#v", tracingInfoByRequestID)
				tracingContext := tracingInfoByRequestID.(jaeger.SpanContext)
				req.Header[jaeger.TraceContextHeaderName] = []string{tracingContext.String()}
				//h.logger.Debugf("Outbound span: %s", tracingContext.String())
			}
			if v := req.Header.Get(config.GetHTTPConfig().XSourceHeaderName); v == "" {
				req.Header.Set(config.GetHTTPConfig().XSourceHeaderName, config.GetHTTPConfig().XSourceValue)
			}
		}

		netHTTPRequest.SetHTTPRequest(req)
		netHTTPRequest.StartRequest()

		bufioWriter := writerPool.Get().(*bufio.Writer)
		bufioWriter.Reset(w)
		// write the same request to writer
		err = req.Write(bufioWriter)
		bufioWriter.Flush()
		writerPool.Put(bufioWriter)
		if err != nil && err != io.ErrUnexpectedEOF {
			h.logger.Errorf("Error while writing request to w: %s", err.Error())
		}
	}

	return w
}

func (h *HTTPHandler) HandleResponse(r *net.TCPConn, w *net.TCPConn, netRequest NetRequest, isInboundConn bool, forceClose bool) {
	netHTTPRequest := netRequest.(*NetHTTPRequest)
	tmpWriter := NewTempWriter()
	defer tmpWriter.Close()
	readerWithFallback := io.TeeReader(r, tmpWriter)
	bufioHTTPReader := readerPool.Get().(*bufio.Reader)
	bufioHTTPReader.Reset(readerWithFallback)
	defer readerPool.Put(bufioHTTPReader)
	if !config.GetHTTPConfig().RoutingEnabled {
		defer netHTTPRequest.CleanUp()
	}
	for {
		tmpWriter.Start()
		resp, err := nhttp.ReadResponse(bufioHTTPReader, nil)
		if err == io.EOF {
			h.logger.Debug("EOF while parsing response HTTP")
			return
		}
		if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
			h.logger.Debug(err.Error())
			return
		}
		if err != nil {
			h.logger.Warningf("Error while parsing http response: %s", err.Error())
			_, err = io.Copy(w, tmpWriter)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			tmpWriter.Stop()
			_, err = io.Copy(w, bufioHTTPReader)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			return
		}

		// avoid ws connections and other upgrade protos
		if strings.ToLower(resp.Header.Get("Connection")) == "upgrade" {
			_, err = io.Copy(w, tmpWriter)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			tmpWriter.Stop()
			_, err = io.Copy(w, bufioHTTPReader)
			if err != nil {
				h.logger.Warning(err.Error())
			}
			return
		}

		tmpWriter.Stop()

		// if method == HEAD and content-length != 0, it will hang on read with LimitReader, handle this:
		rq := netHTTPRequest.httpRequests.Peek()
		if rq != nil && rq.(*nhttp.Request).Method == nhttp.MethodHead {
			// server side can hold connection which leads to stuck Close() method in Write(w)
			if forceClose && resp.StatusCode != 100 {
				r.CloseRead()
				r.CloseWrite()
				r.Close()
			}
			err = resp.Write(w)
		} else {
			bufioWriter := writerPool.Get().(*bufio.Writer)
			bufioWriter.Reset(w)
			// write the same response to w
			err = resp.Write(bufioWriter)
			bufioWriter.Flush()
			writerPool.Put(bufioWriter)
		}

		if err != nil {
			h.logger.Errorf("Error while writing response to w: %s", err.Error())
		}

		netHTTPRequest.SetHTTPResponse(resp)
		netHTTPRequest.StopRequest()
		// in case of 100 response we can't close connection (server can keep on sending responses)
		if forceClose && resp.StatusCode != 100 {
			r.CloseRead()
			r.CloseWrite()
			r.Close()
		}
	}
}

type NetHTTPRequest struct {
	httpRequests          *Queue
	httpResponses         *Queue
	spans                 *Queue
	isInbound             bool
	tracingContextMapping *cache.Cache
	logger                *log.Logger
	remoteAddr            string
}

func NewNetHTTPRequest(logger *log.Logger, isInbound bool, tracingContextMapping *cache.Cache) *NetHTTPRequest {
	return &NetHTTPRequest{
		httpRequests:          NewQueue(),
		httpResponses:         NewQueue(),
		spans:                 NewQueue(),
		logger:                logger,
		isInbound:             isInbound,
		tracingContextMapping: tracingContextMapping,
	}
}

func (nr *NetHTTPRequest) StartRequest() {
	request := nr.httpRequests.Peek()
	if request == nil {
		return
	}
	httpRequest := request.(*nhttp.Request)
	carrier := opentracing.HTTPHeadersCarrier(httpRequest.Header)

	wireContext, err := opentracing.GlobalTracer().Extract(opentracing.HTTPHeaders, carrier)

	operation := httpRequest.URL.Path
	if !nr.isInbound {
		operation = httpRequest.Host + httpRequest.URL.Path
	}
	httpConfig := config.GetHTTPConfig()
	var span opentracing.Span
	if err != nil {
		nr.logger.Infof("Carrier extract error: %s", err.Error())
		span = opentracing.StartSpan(
			operation,
		)

		if nr.isInbound {
			context := span.Context().(jaeger.SpanContext)
			nr.tracingContextMapping.SetDefault(
				httpRequest.Header.Get(httpConfig.RequestIdHeaderName),
				context,
			)

			if len(httpConfig.HeadersMap) > 0 {
				// prefer httpConfig iteration, headers are already parsed into a map
				for headerName, tagName := range httpConfig.HeadersMap {
					if val := httpRequest.Header.Get(headerName); val != "" {
						span.SetTag(tagName, val)
					}
				}
			}
			if len(httpConfig.CookiesMap) > 0 {
				// prefer cookies list iteration (there is no pre-parsed cookies list)
				for _, cookie := range httpRequest.Cookies() {
					if tagName, ok := httpConfig.CookiesMap[cookie.Name]; ok {
						span.SetTag(tagName, cookie.Value)
					}
				}
			}
		} else {
			span.Tracer().Inject(
				span.Context(),
				opentracing.HTTPHeaders,
				opentracing.HTTPHeadersCarrier(httpRequest.Header),
			)
		}
	} else {
		span = opentracing.StartSpan(
			operation,
			opentracing.ChildOf(wireContext),
		)

		if nr.isInbound {
			context := span.Context().(jaeger.SpanContext)
			nr.tracingContextMapping.SetDefault(
				httpRequest.Header.Get(httpConfig.RequestIdHeaderName),
				context,
			)
		}
	}

	nr.spans.Push(span)
}

func (nr *NetHTTPRequest) StopRequest() {
	request := nr.httpRequests.Pop()
	response := nr.httpResponses.Pop()
	if request != nil && response != nil {
		httpRequest := request.(*nhttp.Request)
		httpResponse := response.(*nhttp.Response)
		span := nr.spans.Pop()
		if span != nil {
			requestSpan := span.(opentracing.Span)
			nr.fillSpan(requestSpan, httpRequest, httpResponse)
			requestSpan.Finish()
		}
	}

	if request != nil && response == nil {
		httpRequest := request.(*nhttp.Request)
		span := nr.spans.Pop()
		if span != nil {
			requestSpan := span.(opentracing.Span)
			nr.fillSpan(requestSpan, httpRequest, nil)
			requestSpan.SetTag("error", true)
			requestSpan.SetTag("timeout", true)
			requestSpan.Finish()
		}
	}
}

func (nr *NetHTTPRequest) CleanUp() {
	// here we can do some cleanup staff
}

func (nr *NetHTTPRequest) fillSpan(
	span opentracing.Span,
	req *nhttp.Request,
	resp *nhttp.Response) {
	if nr.isInbound {
		span.SetTag("span.kind", "server")
	} else {
		span.SetTag("span.kind", "client")
	}
	span.SetTag("remote_addr", nr.remoteAddr)
	if req != nil {
		span.SetTag("http.host", req.Host)
		span.SetTag("http.path", req.URL.String())
		span.SetTag("http.request_size", req.ContentLength)
		span.SetTag("http.method", req.Method)
		if userAgent := req.Header.Get("User-Agent"); userAgent != "" {
			span.SetTag("http.user_agent", userAgent)
		}
		if requestID := req.Header.Get(config.GetHTTPConfig().RequestIdHeaderName); requestID != "" {
			span.SetTag("http.request_id", requestID)
		}
	}
	if resp != nil {
		span.SetTag("http.response_size", resp.ContentLength)
		span.SetTag("http.status_code", resp.StatusCode)
		if resp.StatusCode >= 500 {
			span.SetTag("error", "true")
		}
	}
}

func (nr *NetHTTPRequest) SetHTTPRequest(r *nhttp.Request) {
	nr.httpRequests.Push(r)
}

func (nr *NetHTTPRequest) SetHTTPResponse(r *nhttp.Response) {
	nr.httpResponses.Push(r)
}

// NewQueue creates new queue
func NewQueue() *Queue {
	return &Queue{
		elements: list.New(),
	}
}

// Queue implements queue data structure
type Queue struct {
	mu       sync.Mutex
	elements *list.List
}

// Push pushes element to the end of queue
func (q *Queue) Push(value interface{}) {
	q.mu.Lock()
	q.elements.PushBack(value)
	q.mu.Unlock()
}

// Pop pops first element out of queue
func (q *Queue) Pop() interface{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	el := q.elements.Front()
	if el == nil {
		return nil
	}
	return q.elements.Remove(el)
}

// Peek returns first element in the queue without removing it
func (q *Queue) Peek() interface{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	if el := q.elements.Front(); el != nil {
		return el.Value
	} else {
		return nil
	}
}

// Clear clears queue
func (q *Queue) Clear() {
	for el := q.Pop(); el != nil; {
	}
}

func getRoutingDestination(routingValue string, host string, originalDst string) (string, error) {
	pairs := strings.Split(routingValue, ",")
	for _, p := range pairs {
		keyval := strings.Split(p, "=")
		if len(keyval) < 2 {
			return "", fmt.Errorf("malformed routing header: '%s'", routingValue)
		}
		// avoid infinite route loops
		if keyval[0] == keyval[1] {
			continue
		}
		if host == keyval[0] {
			if !strings.Contains(keyval[1], ":") {
				keyval[1] += ":80"
			}
			return keyval[1], nil
		}
	}
	return originalDst, nil
}
