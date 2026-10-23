// Package helmmap traces a drifted Helm-rendered value back to the values
// file that feeds it. It is pure logic: it works on file contents and an
// Application's Helm source settings and never talks to Argo CD or a cluster.
package helmmap
