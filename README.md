
ellycache is a simple, performant, query cache for PostgreSQL with a built-in
HTTP server. It runs the queries you specify at cron-like schedules, caches the
results in-memory or on-disk and serves them at specified HTTP endpoints using
the built-in webserver.

ellycache was built to balance the needs of BI/analytics apps that access
PostgreSQL data, and PostgreSQL DBAs who need to manage the system resources
consumed by PostgreSQL. The cron-like scheduling of SQL queries, together with
a compact single binary deployment that includes an HTTP server, connection
pooler and on-disk encrypted file cache make ellycache a compelling alternative
to traditional query caching software.

You can start ellycache with a configuration file listing the HTTP URI endpoints
together with the SQL query and run schedule. Here is an example configuration
file:

```hcl
listen = ":8080" # host and port for the HTTP server to listen on

# define the postgres server to connect to. ellycache includes a
# built-in Postgres connection pooler.
connection {
	dsn = "host=10.2.2.1 port=5433 user=analyticsro dbname=pagila"
	maxconns = 5
	idletimeout = "10m"
}

# define a URI that you can do HTTP GET from, which will run the SQL query
endpoint "/rentals/late" {
	sql = <<EOLATE
SELECT
	CONCAT(customer.last_name, ', ', customer.first_name) AS customer,
	address.phone,
	film.title
FROM
	rental
	INNER JOIN customer ON rental.customer_id = customer.customer_id
	INNER JOIN address ON customer.address_id = address.address_id
	INNER JOIN inventory ON rental.inventory_id = inventory.inventory_id
	INNER JOIN film ON inventory.film_id = film.film_id
WHERE
	rental.return_date IS NULL
	AND rental_date < CURRENT_DATE
ORDER BY
	title
EOLATE

	schedule = "*/5 * * * *"  # cron schedule for running the query 
	rowformat = "object"      # HTTP json body is an array of objects
	filebacked = true         # cache results in an encrypted temp file
}
```

Then start ellycache like this (the `-d` enables debug output):

```
% ellycache -d cache.cfg
2024/11/14 10:47:39 debug: scheduled /rentals/late, next at 2024-11-14 10:50:00
2024/11/14 10:50:00 debug: database query for /rentals/late took 24.397834ms
2024/11/14 10:50:00 debug: populated result for /rentals/late
2024/11/14 10:53:28 debug: "/rentals/late" 200 195.875µs
2024/11/14 10:55:00 debug: database query for /rentals/late took 9.564084ms
2024/11/14 10:55:00 debug: replaced result for /rentals/late (no change in content)
```

After the data is populated for the first time, you can GET the URI to see the result:

```
% curl -i http://localhost:8080/rentals/late
HTTP/1.1 200 OK
Cache-Control: max-age=299, immutable
Content-Type: application/json
Etag: W/"e336cbe3520364bc"
Last-Modified: Thu, 14 Nov 2024 05:20:00 GMT
Vary: Accept-Encoding
Date: Thu, 14 Nov 2024 05:23:28 GMT
Transfer-Encoding: chunked

[
  {"customer":"OLVERA, DWAYNE","phone":"62127829280","title":"ACADEMY DINOSAUR"},
  {"customer":"HUEY, BRANDON","phone":"99883471275","title":"ACE GOLDFINGER"},
  {"customer":"OWENS, CARMEN","phone":"272234298332","title":"AFFAIR PREJUDICE"},
  {"customer":"HANNON, SETH","phone":"864392582257","title":"AFRICAN EGG"},
  {"customer":"COLE, TRACY","phone":"371490777743","title":"ALI FOREVER"},
...snip...
  {"customer":"LEONE, LOUIS","phone":"45554316010","title":"ZHIVAGO CORE"}
]
```

Note the `Cache-Control`, `Etag` and other headers that ensure ellycache can be
fronted efficiently by CDN servers or browsers.

Array-of-array JSON format is also supported for less verbose response.

### Installation

ellycache is a single, self-contained, zero-dependency binary. You can download
the latest release from the [releases](https://github.com/rapidloop/ellycache/releases)
page. If you have a working recent Go toolchain installed, you can also build
it yourself:

```shell
$ go install github.com/rapidloop/ellycache@latest
```

### Getting Started

ellycache is started with a configuration file as an argument. You can ask
ellycache to print an example configuration to get going quickly:

```
$ ellycache --example > cache.cfg
```

Edit the config file as required, then start ellycache with it:

```
$ ellycache cache.cfg
```

Include the `-d/--debug` flag to see what ellycache is doing:

```
$ ellycache --debug cache.cfg
```

### Features

#### Isolation of HTTP requests and PostgreSQL load

ellycache deliberately does not query on first request and then cache the result
for later requests. By running SQL queries only as per a fixed schedule, the
load on the PostgreSQL server, tracking of long running queries, usage of
resources by expensive analytics queries all become easier to predict and
manage.

The results of the last expensive analytics query are available to BI apps and
the like until it becomes reasonable to do another query. The freshness of the
data can be seen from the `Last-Modified` header.

This sets an (adjustable) balance between BI/analytics web apps needs and
database server load management.

#### Simple configuration

ellycache is configured with a simple nginx-like configuration file. An example
configuration file can be printed out with `ellycache -e` to serve as both a
starting point and documentation.

#### Easy to deploy

ellycache is a single static binary with no dependencies. You can deploy it into
VMs or containers or even bundle it with your apps easily. It is written in
pure Go, and can be built for any platform that [Go supports]
(https://go.dev/wiki/MinimumRequirements).

#### PostgreSQL connection pooling

ellycache maintains an internal PostgreSQL connection pool to limit resource
usage by concurrent queries and reuse connections between quickly repeating
queries. You can configure the *maximum concurrent connections* and
the *maximum duration that a connection is allowed to be idle before closing
it* in the ellycache configuration file.

#### Encrypted file-backed caches if needed

If the result of a query is too large and memory usage is a concern, you can ask
ellycache to save the result to a file instead, on a per-endpoint basis. Such
files are AES-256-GCM encrypted with an ephermal key that is valid only for the
current ellycache process lifetime. Only the ellycache process that created the
file can decrypt them, and if that process crashes, then no one can.

### Support

ellycache is an open-source project from [RapidLoop]
(https://rapidloop.com) , the makers of [pgDash](https://pgdash.io]). It is
currently hosted at [GitHub](https://github.com/rapidloop/ellycache). Community
support is available via [discussions](https://github.com/rapidloop/ellycache/discussions).
Feel free to [raise issues you encounter](https://github.com/rapidloop/ellycache/issues) or
[discuss improvements](https://github.com/rapidloop/ellycache/discussions). For
paid support or custom features for ellycache, do [contact us](mailto:hello@rapidloop.com).
