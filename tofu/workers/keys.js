export default {
  fetch(request, env) {
    const url = new URL(request.url);
    if (url.protocol === "http:") {
      url.protocol = "https:";
      return Response.redirect(url.href, 301);
    }
    if (url.pathname !== "/") {
      return new Response("not found\n", { status: 404 });
    }
    return new Response(env.KEYS, {
      headers: { "content-type": "text/plain; charset=utf-8" },
    });
  },
};
