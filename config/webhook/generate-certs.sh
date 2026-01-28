#!/bin/bash

# Generate self-signed certificates for webhook
set -e

NAMESPACE="pod-sequence-system"
SERVICE="pod-sequence-webhook"
SECRET="webhook-server-cert"

# Create a temporary directory for certs
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

cd $TMPDIR

# Generate CA private key
openssl genrsa -out ca.key 2048

# Generate CA certificate
openssl req -x509 -new -nodes -key ca.key -subj "/CN=${SERVICE}.${NAMESPACE}.svc" -days 10000 -out ca.crt

# Generate server private key
openssl genrsa -out server.key 2048

# Create certificate signing request
cat >csr.conf <<EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SERVICE}
DNS.2 = ${SERVICE}.${NAMESPACE}
DNS.3 = ${SERVICE}.${NAMESPACE}.svc
DNS.4 = ${SERVICE}.${NAMESPACE}.svc.cluster.local
EOF

# Generate certificate signing request
openssl req -new -key server.key -subj "/CN=${SERVICE}.${NAMESPACE}.svc" -out server.csr -config csr.conf

# Sign the certificate
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -days 10000 -extensions v3_req -extfile csr.conf

# Create or update secret in Kubernetes
kubectl create secret tls ${SECRET} \
  --cert=server.crt \
  --key=server.key \
  --namespace=${NAMESPACE} \
  --dry-run=client -o yaml | kubectl apply -f -

# Get the CA bundle and update webhook configuration
CA_BUNDLE=$(cat ca.crt | base64 | tr -d '\n')

echo ""
echo "Certificate created. Update the webhook configuration with this CA bundle:"
echo ""
echo "caBundle: ${CA_BUNDLE}"
echo ""
echo "Or run this command to patch it automatically:"
echo ""
echo "kubectl patch mutatingwebhookconfiguration podsequence-mutating-webhook --type='json' -p='[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\":\"${CA_BUNDLE}\"}]'"
