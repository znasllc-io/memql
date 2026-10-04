package node

// BoundAddrForTest is the address NodeServer's listener actually bound, for a
// test that configured MEMQL_NODE_SERVICE_ADDRESS=127.0.0.1:0 and has to dial
// what the kernel picked (Address() answers the configured value). Valid only
// between Ready and Stop.
func (s *NodeServer) BoundAddrForTest() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}
