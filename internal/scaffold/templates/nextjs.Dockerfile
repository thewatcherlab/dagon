# Written by dagon for Next.js. Needs output: "standalone" in next.config.
# The container must answer HTTP on port 3000.
FROM node:{%.Node%}-alpine AS deps
WORKDIR /app
COPY {%.PM.Copy%} ./
RUN {%.PM.Install%}

FROM node:{%.Node%}-alpine AS build
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
COPY --from=deps /app/node_modules ./node_modules
COPY . .
# Public values only: NEXT_PUBLIC_* are baked into the browser bundle.
RUN set -a && if [ -f deploy/build.env ]; then . ./deploy/build.env; fi && set +a && {%.PM.Build%}

FROM node:{%.Node%}-alpine AS run
WORKDIR /app
ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1 \
    HOSTNAME=0.0.0.0 \
    PORT=3000
{%- if .HasPublic%}
COPY --from=build --chown=node:node /app/public ./public
{%- end%}
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
USER node
EXPOSE 3000
CMD ["node", "server.js"]
