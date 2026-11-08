# Written by dagon for a Node server (Nuxt, Remix, SvelteKit, Astro, Express...).
# `{%.PM.Name%} start` must serve HTTP on $PORT, which is 3000.
FROM node:{%.Node%}-alpine AS build
WORKDIR /app
COPY {%.PM.Copy%} ./
RUN {%.PM.Install%}
COPY . .
{%- if .HasBuild%}
# Public values only: anything a client bundle reads ends up in the browser.
RUN set -a && if [ -f .infra/build.env ]; then . ./.infra/build.env; fi && set +a && {%.PM.Build%}
{%- end%}
{%- if .PM.Prune%}
RUN {%.PM.Prune%}
{%- end%}

FROM node:{%.Node%}-alpine
WORKDIR /app
ENV NODE_ENV=production \
    HOST=0.0.0.0 \
    HOSTNAME=0.0.0.0 \
    PORT=3000
COPY --from=build --chown=node:node /app ./
USER node
EXPOSE 3000
CMD ["npm", "start"]
