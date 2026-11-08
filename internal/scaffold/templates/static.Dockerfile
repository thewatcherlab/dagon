# Written by dagon for a static site. The build writes {%.OutputDir%}/, and nginx
# serves it on port 3000, falling back to index.html for client-side routes.
FROM node:{%.Node%}-alpine AS build
WORKDIR /app
COPY {%.PM.Copy%} ./
RUN {%.PM.Install%}
COPY . .
RUN set -a && if [ -f .infra/build.env ]; then . ./.infra/build.env; fi && set +a && {%.PM.Build%}

FROM nginxinc/nginx-unprivileged:stable-alpine
COPY .infra/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/{%.OutputDir%} /usr/share/nginx/html
EXPOSE 3000
