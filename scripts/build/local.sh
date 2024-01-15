FILE=agent.codefly.yaml
AGENT=$( yq e '.name' $FILE)
VERSION=$(yq e '.version' $FILE)

echo Building ${AGENT}:${VERSION}
go build -gcflags="all=-N -l" -o ~/.codefly/agents/services/codefly.dev/${AGENT}__${VERSION} *.go
