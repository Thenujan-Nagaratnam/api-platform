FROM ghcr.io/wso2/api-platform/gateway-controller:0.0.1

# Copy policy definitions
RUN rm -rf /app/policies
COPY policies /app/policies

# Set environment variable for policy directory
ENV GATEWAY_POLICIES_DEFINITIONS__PATH=/app/policies

# Metadata labels
LABEL org.opencontainers.image.title="Gateway Controller with Custom Policies"
LABEL org.opencontainers.image.description="Gateway Controller extended with custom policy definitions"
LABEL build.timestamp="2025-12-05T09:42:53Z"
LABEL build.builder-version="v1.0.0"
LABEL build.base-image="ghcr.io/wso2/api-platform/gateway-controller:0.0.1"
LABEL build.builder-version="v1.0.0"
LABEL build.policy-count="2"
LABEL build.timestamp="2025-12-05T09:42:53Z"
