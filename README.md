SafeSQL
=======

SafeSQL is a static analysis tool for Go that protects against SQL injections.

This version is a fork from github.com/stripe/safesql with pgx v4 support added.

Usage
-----

```
$ go install github.com/intrinsec/safesql@latest

$ safesql
Usage: safesql [-q] [-v] package1 [package2 ...]
  -q=false: Only print on failure
  -v=false: Verbose mode

$ safesql example.com/an/unsafe/package
Found 1 potentially unsafe SQL statements:
- /Users/alice/go/src/example.com/an/unsafe/package/db.go:14:19
Please ensure that all SQL queries you use are compile-time constants.
You should always use parameterized queries or prepared statements
instead of building queries from strings.

$ safesql example.com/a/safe/package
You're safe from SQL injection! Yay \o/
```


How does it work?
-----------------

SafeSQL uses the static analysis utilities in [go/tools][tools] to search for
all call sites of each of the `query` functions in packages ([database/sql][sql],[github.com/jinzhu/gorm][gorm],[github.com/jmoiron/sqlx][sqlx])
(i.e., functions which accept a parameter named `query`,`sql`). It then makes
sure that every such call site uses a query that is a compile-time constant.

The principle behind SafeSQL's safety guarantees is that queries that are
compile-time constants cannot be subverted by user-supplied data: they must
either incorporate no user-controlled values, or incorporate them using the
package's safe placeholder mechanism. In particular, call sites which build up
SQL statements via `fmt.Sprintf` or string concatenation or other mechanisms
will not be allowed.

[tools]: https://godoc.org/golang.org/x/tools/go
[sql]: http://golang.org/pkg/database/sql/
[sqlx]: https://github.com/jmoiron/sqlx
[gorm]: https://github.com/jinzhu/gorm

False positives
---------------

If SafeSQL passes, your application is free from SQL injections (modulo bugs in
the tool), however there are a great many safe programs which SafeSQL will
declare potentially unsafe. These false positives fall roughly into two buckets:

First, SafeSQL does not currently recursively trace functions through the call
graph. If you have a function that looks like this:

    func MyQuery(query string, args ...interface{}) (*sql.Rows, error) {
            return globalDBObject.Query(query, args...)
    }

and only call `MyQuery` with compile-time constants, your program is safe;
however SafeSQL will report that `(*database/sql.DB).Query` is called with a
non-constant parameter (namely the parameter to `MyQuery`). This is by no means
a fundamental limitation: SafeSQL could recursively trace the `query` argument
through every intervening helper function to ensure that its argument is always
constant, but this code has yet to be written.


The second sort of false positive is based on a limitation in the sort of
analysis SafeSQL performs: there are many safe SQL statements which are not
feasible (or not possible) to represent as compile-time constants. More advanced
static analysis techniques (such as taint analysis).

In order to ignore false positives, add the following comment to the line before
or the same line as the statement:
```
//nolint:safesql
```

Even if a statement is ignored it will still be logged, but will not cause 
safesql to exit with a status code of 1 if all found statements are ignored.

Adding tests
---------------
To add a test create a new director in `testdata` and add a go program in the 
folder you created, for an example look at `testdata/multiple_files`.

After adding a new directory and go program, add an entry to the tests map in 
`safesql_test.go`, which will run the tests against the program added.
