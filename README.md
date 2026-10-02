# caddy-cloudfront-ip

A Caddy IP source of CloudFront's origin-facing ranges, for `trusted_proxies`.

It reads the `CLOUDFRONT_ORIGIN_FACING` prefixes from
[ip-ranges.amazonaws.com] when Caddy starts and every 12 hours after that, so
Caddy trusts the `X-Forwarded-For` that CloudFront adds without a hand-kept
list.

## Install

```sh
xcaddy build --with github.com/femiwiki/caddy-cloudfront-ip
```

## Usage

```caddyfile
{
	servers {
		trusted_proxies cloudfront
		trusted_proxies_strict
	}
}
```

All subdirectives are optional:

```caddyfile
trusted_proxies cloudfront {
	interval 12h
	timeout 30s
	fallback 3.172.0.0/18 15.158.0.0/16
}
```

- **interval** is how often the list is fetched again.
- **timeout** is how long one fetch may take.
- **fallback** is the list used when the first fetch fails. Without it, Caddy
  refuses to start on a failed first fetch.

A failed fetch after the first keeps the last list.

## License

[Apache License 2.0](LICENSE)

[ip-ranges.amazonaws.com]: https://ip-ranges.amazonaws.com/ip-ranges.json
