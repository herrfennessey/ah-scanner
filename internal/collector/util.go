package collector

import "net/url"

func urlNS(ns string) url.Values { return url.Values{"namespace": {ns}} }
