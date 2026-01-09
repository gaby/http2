# HTTP/2 RFC 7540 Compliance Improvement Plan

## Executive Summary

The **dgrr/http2** library is already highly compliant with RFC 7540/7541, with 103+ h2spec tests passing and strong performance (4.2x faster than net/http2). However, there are specific areas where compliance can be enhanced, particularly around header validation, trailers support, and edge case handling.

**Current Status**: ~85% RFC 7540 compliance
**Target Status**: 100% RFC 7540 compliance
**Estimated Timeline**: 6-10 weeks

---

## Priority 1: Critical Compliance Gaps

### 1.1 CONTINUATION Frame Handling (RFC 7540 Section 6.10)
**Status**: ❌ Tests disabled (http2/6.10/4-5)
**Location**: `serverConn.go:1155-1178`, `h2spec/h2spec_test.go:160-161`
**Issue**: Stream is processed immediately upon receiving END_HEADERS flag, ignoring subsequent CONTINUATION frames

**RFC Requirement**:
> "A receiver MUST be prepared to receive one or more CONTINUATION frames for any HEADERS or PUSH_PROMISE frame."

**Impact**: Protocol violation when clients send CONTINUATION after END_HEADERS

**Action Items**:
- [ ] Modify `handleHeaderFrame` in `serverConn.go:1174` to buffer header fragments until true END_HEADERS
- [ ] Track header continuation state per stream
- [ ] Delay stream processing until all CONTINUATION frames received
- [ ] Add validation to reject non-CONTINUATION frames during header continuation
- [ ] Enable h2spec tests http2/6.10/4-5

**Estimated Complexity**: Medium (2-3 days)

---

### 1.2 Header Field Validation (RFC 7540 Section 8.1.2)
**Status**: ❌ 13 tests disabled due to fasthttp case-insensitivity
**Location**: `h2spec/h2spec_test.go:168-183`
**Tests Affected**:
- 8.1.2.1/1-2, 4: Uppercase header field names
- 8.1.2.2/1-2: Connection-specific header fields
- 8.1.2.3/1-7: Pseudo-header validation
- 8.1.2.6/1-2: TE header validation
- 8.1.2/1: General header field validation

**RFC Requirements**:
> "A request or response containing uppercase header field name MUST be treated as malformed (Section 8.1.2.6)"

**Impact**: Allows malformed requests/responses that should be rejected

**Action Items**:
- [ ] Implement case-sensitivity check in HPACK decoder (`hpack.go:269-358`)
- [ ] Add validation for connection-specific headers (Connection, Keep-Alive, Proxy-Connection, Transfer-Encoding, Upgrade)
- [ ] Validate pseudo-headers (:method, :path, :scheme, :authority):
  - Must appear before regular headers
  - Must not appear in trailers
  - Cannot be duplicated
  - Must be lowercase
- [ ] Validate TE header (only "trailers" value allowed)
- [ ] Return ProtocolError for violations
- [ ] Enable all 13 disabled h2spec tests in section 8.1.2.x

**Estimated Complexity**: High (4-5 days)

---

### 1.3 HTTP/2 Trailers Support (RFC 7540 Section 8.1.3)
**Status**: ❌ Not implemented
**Location**: `serverConn.go:1176` (TODO comment)

**RFC Requirement**:
> "Trailer header fields are carried in a header block that also terminates the stream."

**Impact**: Cannot support trailing headers for chunked responses

**Action Items**:
- [ ] Detect trailers (HEADERS frame with END_STREAM but no END_HEADERS initially)
- [ ] Store trailer headers separately from leading headers
- [ ] Expose trailers in fasthttp.RequestCtx/Response
- [ ] Add `Stream.trailers` field
- [ ] Validate that trailers don't contain pseudo-headers
- [ ] Add tests for trailer scenarios
- [ ] Document trailers support in README

**Estimated Complexity**: Medium (3-4 days)

---

## Priority 2: Important Compliance Issues

### 2.1 Stream State Management for Reserved Streams (RFC 7540 Section 5.1.2)
**Status**: ⚠️ Test disabled (http2/5.1.2/1)
**Location**: `h2spec/h2spec_test.go:92`

**RFC Requirement**:
> "Endpoints MUST NOT exceed the limit set by their peer."

**Action Items**:
- [ ] Investigate why test http2/5.1.2/1 is disabled
- [ ] Review stream state transitions for reserved state (`stream.go:31-58`)
- [ ] Ensure proper handling of PUSH_PROMISE stream reservation
- [ ] Validate stream ID parity (client: odd, server: even)
- [ ] Enable the disabled test

**Estimated Complexity**: Low (1-2 days)

---

### 2.2 GOAWAY Connection Closure Behavior (RFC 7540 Section 5.4.1)
**Status**: ⚠️ Non-compliant behavior documented
**Location**: `h2spec/h2spec_test.go:108-110`

**RFC Requirement**:
> "After sending a GOAWAY frame, the sender can discard frames for streams initiated by the receiver with identifiers higher than the identified last stream."

**Current Behavior**: Sends GOAWAY but expects h2spec to close connection

**Action Items**:
- [ ] Review GOAWAY handling in `serverConn.go:612-622`
- [ ] Implement graceful connection closure after GOAWAY
- [ ] Add timeout for connection closure (e.g., 5 seconds)
- [ ] Ensure pending streams below last stream ID complete
- [ ] Test with h2spec test http2/5.4.1/1

**Estimated Complexity**: Medium (2-3 days)

---

### 2.3 Push Promise Implementation Completeness
**Status**: ⚠️ Incomplete with TODOs
**Location**: `serverConn.go:1043`, `serverConn.go:1045`

**Action Items**:
- [ ] Complete StreamStateReserved handling in `handleDataFrame`
- [ ] Implement proper push promise flow for reserved streams
- [ ] Add validation for push promise frame fields
- [ ] Test push promise with h2spec tests
- [ ] Document push promise limitations if any

**Estimated Complexity**: Medium (2-3 days)

---

## Priority 3: Code Quality & Optimizations

### 3.1 DATA Frame Padding Support
**Status**: 🔧 TODO at `data.go:104`

**Action Items**:
- [ ] Implement padding generation in `Data.Serialize`
- [ ] Add padding length field to Data struct
- [ ] Test padding with flow control calculations
- [ ] Ensure padding bytes counted against window size

**Estimated Complexity**: Low (1 day)

---

### 3.2 Frame Size Validation
**Status**: 🔧 TODO at `frameHeader.go:216`

**RFC Requirement**:
> "An endpoint MUST send an error code of FRAME_SIZE_ERROR if a frame exceeds the size defined by SETTINGS_MAX_FRAME_SIZE."

**Action Items**:
- [ ] Implement frame size checking in `FrameHeader.readFrom`
- [ ] Discard oversized frame bytes
- [ ] Return FrameSizeError to close stream/connection
- [ ] Add test for max frame size violation

**Estimated Complexity**: Low (1 day)

---

### 3.3 HPACK Optimizations (Non-Compliance)
**Status**: 🔧 Performance improvements
**Locations**:
- `huffman.go:14` - SSE optimization
- `hpack.go:112` - Reverse index optimization
- `hpack.go:469` - Conditional Huffman encoding

**Action Items**:
- [ ] Profile HPACK encoding/decoding performance
- [ ] Implement reverse index lookup for dynamic table
- [ ] Add conditional Huffman encoding (encode only if shorter)
- [ ] Consider SSE/SIMD for Huffman decoding (Go 1.18+ with assembly)
- [ ] Benchmark improvements

**Estimated Complexity**: High (5-7 days) - Optional

---

### 3.4 Code Cleanup & Documentation

**Minor TODOs**:
- [ ] `frameHeader.go:30` - Develop methods for FrameFlags
- [ ] `frameHeader.go:185` - Remove unused rb parameter
- [ ] `conn.go:42` - Expand Handshake function documentation
- [ ] `conn.go:434` - Use atomic operations for stream setting
- [ ] `conn.go:583` - Add panic for unexpected state
- [ ] `goaway.go:54` - Add error descriptions to GOAWAY debug data
- [ ] `goaway.go:79` - Clarify unclear comment
- [ ] `hpack.go:65` - Rename AcquireHPACK function
- [ ] `hpack.go:489` - Change naming convention for AppendHeaderField

**Estimated Complexity**: Low (2-3 days total)

---

## Priority 4: Testing & Validation

### 4.1 Expand Test Coverage

**Action Items**:
- [ ] Add tests for trailer handling scenarios
- [ ] Add tests for malformed header validation
- [ ] Add tests for edge cases in CONTINUATION frame handling
- [ ] Add integration tests with various HTTP/2 clients (curl, browsers)
- [ ] Add benchmarks for new features
- [ ] Test with h2spec in strict mode (all tests enabled)

**Estimated Complexity**: Medium (3-4 days)

---

### 4.2 Documentation Updates

**Action Items**:
- [ ] Document RFC compliance status in README
- [ ] List known limitations explicitly
- [ ] Add compliance badge/section
- [ ] Document which h2spec tests pass/fail and why
- [ ] Create COMPLIANCE.md with detailed RFC mapping
- [ ] Update IMPLEMENTATION.md with new features

**Estimated Complexity**: Low (1-2 days)

---

## Implementation Roadmap

### Phase 1: Critical Fixes (2-3 weeks)
1. Header field validation (8.1.2.x)
2. CONTINUATION frame handling (6.10)
3. Trailers support (8.1.3)

**Deliverables**:
- 13 additional h2spec tests passing
- Full header validation
- Trailers support with tests

### Phase 2: Important Improvements (1-2 weeks)
4. Reserved stream state management (5.1.2)
5. GOAWAY connection closure (5.4.1)
6. Push promise completion

**Deliverables**:
- 3 additional h2spec tests passing
- Complete push promise support

### Phase 3: Polish & Optimization (1-2 weeks)
7. DATA frame padding
8. Frame size validation
9. Code cleanup
10. Documentation updates

**Deliverables**:
- All TODOs resolved
- Updated documentation
- 100% h2spec compliance

### Phase 4: Optional Performance (1-2 weeks)
11. HPACK optimizations
12. Comprehensive testing
13. Benchmarking

**Deliverables**:
- Performance improvements
- Comprehensive benchmarks

---

## Success Criteria

✅ **Primary Goals**:
- All disabled h2spec tests enabled and passing
- 100% h2spec compliance in sections 6.10 and 8.1.2.x
- HTTP/2 trailers fully supported
- Zero known RFC violations

✅ **Secondary Goals**:
- Improved error messages with RFC section references
- Comprehensive test coverage (>90%)
- Updated documentation reflecting compliance status
- Performance maintained or improved

---

## Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|-----------|
| Breaking changes to API | Medium | High | Add feature flags for strict mode |
| Performance regression | Low | Medium | Benchmark before/after |
| Fasthttp compatibility | Medium | High | Work with fasthttp team on case-sensitivity |
| Test flakiness | Low | Low | Use deterministic timeouts |

---

## Dependencies

**External**:
- May require changes to fasthttp for case-sensitive header handling
- H2spec tool for validation

**Internal**:
- Stream state machine refactoring
- HPACK decoder modifications
- Frame processing pipeline updates

---

## Known Limitations & Notes

### 1. Fasthttp Case-Sensitivity
The biggest blocker is fasthttp's case-insensitive header handling. This may require:
- Forking fasthttp's header parsing for HTTP/2
- Contributing case-sensitive mode to fasthttp upstream
- Adding validation layer before fasthttp processing

### 2. Backward Compatibility
Consider adding a `StrictRFC` configuration flag to enable/disable strict validation:
```go
type ServerConfig struct {
    StrictRFC bool // Enable strict RFC 7540 compliance (may break existing clients)
}
```

This allows users to opt-in to strict mode without breaking existing deployments.

### 3. Performance Considerations
All changes should maintain or improve the current 4.2x performance advantage over net/http2:
- Benchmark before/after each change
- Profile hot paths
- Consider lazy validation (only when needed)

---

## Disabled H2Spec Tests Summary

| Test ID | RFC Section | Reason | Priority |
|---------|-------------|--------|----------|
| http2/5.1.2/1 | 5.1.2 Stream Identifiers | Unknown | P2 |
| http2/6.10/4-5 | 6.10 CONTINUATION | Early stream processing | P1 |
| http2/8.1.2.1/1-2,4 | 8.1.2.1 Pseudo-Header Fields | Case sensitivity | P1 |
| http2/8.1.2.2/1-2 | 8.1.2.2 Connection-Specific Header Fields | Not validated | P1 |
| http2/8.1.2.3/1-7 | 8.1.2.3 Request Pseudo-Header Fields | Not validated | P1 |
| http2/8.1.2.6/1-2 | 8.1.2.6 Malformed Requests and Responses | Not validated | P1 |
| http2/8.1.2/1 | 8.1.2 HTTP Header Fields | General validation | P1 |

**Total Disabled**: 18 tests
**Target**: Enable and pass all 18 tests

---

## References

- [RFC 7540: Hypertext Transfer Protocol Version 2 (HTTP/2)](https://tools.ietf.org/html/rfc7540)
- [RFC 7541: HPACK: Header Compression for HTTP/2](https://tools.ietf.org/html/rfc7541)
- [h2spec: Conformance testing tool for HTTP/2](https://github.com/summerwind/h2spec)
- [fasthttp: Fast HTTP implementation for Go](https://github.com/valyala/fasthttp)

---

## Change Log

- **2026-01-09**: Initial compliance plan created
- **TBD**: Phase 1 completion
- **TBD**: Phase 2 completion
- **TBD**: Phase 3 completion
- **TBD**: 100% compliance achieved

---

## Contact & Contributions

For questions or contributions to this compliance effort, please:
1. Open an issue on GitHub
2. Reference this plan in your PR description
3. Include h2spec test results with your changes
4. Update this document as items are completed
