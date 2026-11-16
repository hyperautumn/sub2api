System.register(["react", "@grafana/data", "@grafana/ui"], function (exports) {
  let React;
  let PanelPlugin;
  let Button;
  let useTheme2;

  async function copyText(text) {
    if (navigator.clipboard?.writeText) {
      try {
        await navigator.clipboard.writeText(text);
        return;
      } catch {
        // The browser may restrict Clipboard API access.
      }
    }

    const previous = document.activeElement;
    const input = document.createElement("textarea");
    input.value = text;
    input.readOnly = true;
    input.style.position = "fixed";
    input.style.opacity = "0";
    document.body.appendChild(input);
    try {
      input.select();
      if (!document.execCommand("copy")) {
        throw new Error("Copy failed");
      }
    } finally {
      input.remove();
      previous?.focus();
    }
  }

  function Attempt({ id, method, text, height }) {
    const [status, setStatus] = React.useState("idle");
    const theme = useTheme2();
    const h = React.createElement;
    const copy = async () => {
      setStatus("copying");
      try {
        await copyText(text);
        setStatus("copied");
      } catch {
        setStatus("failed");
      }
    };
    const statusText = {
      idle: "",
      copying: "正在复制",
      copied: "已复制",
      failed: "复制失败",
    };
    return h(
      "section",
      {
        style: {
          border: "1px solid " + theme.colors.border.weak,
          borderRadius: 6,
          marginBottom: 10,
          overflow: "hidden",
        },
      },
      h(
        "div",
        {
          style: {
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 8,
            padding: "6px 8px",
            borderBottom: "1px solid " + theme.colors.border.weak,
          },
        },
        h("span", { style: { color: theme.colors.text.secondary } }, `尝试 #${id}${method ? " · " + method : ""}`),
        h(
          "div",
          { style: { display: "flex", alignItems: "center", gap: 8 } },
          h("span", { role: "status", "aria-live": "polite" }, statusText[status]),
          h(Button, { icon: "copy", size: "sm", variant: "secondary", onClick: copy, disabled: status === "copying" }, "复制 JSON")
        )
      ),
      h("pre", {
        style: {
          margin: 0,
          padding: 12,
          maxHeight: Math.max(100, height - 55),
          overflow: "auto",
          whiteSpace: "pre-wrap",
          overflowWrap: "anywhere",
          tabSize: 2,
          background: theme.colors.background.secondary,
        },
      }, text)
    );
  }

  function JSONPanel({ data, height }) {
    const attempts = [];
    for (const frame of data.series) {
      const content = frame.fields.find((field) => field.name === "line");
      const ids = frame.fields.find((field) => field.name === "采集序号");
      const methods = frame.fields.find((field) => field.name === "协议 / 方法");
      for (let index = 0; index < frame.length; index += 1) {
        const text = content?.values[index];
        if (typeof text === "string") {
          const id = ids?.values[index] ?? index + 1;
          attempts.push({ key: id, id, method: methods?.values[index], text, height });
        }
      }
    }
    if (!attempts.length) {
      return React.createElement(
        "div", { style: { padding: 12 } },
        data.state === "Loading" ? "正在读取 JSON…" : data.state === "Error" ? "JSON 读取失败" : "该请求没有已采集的 JSON。"
      );
    }
    return React.createElement(
      "div", { style: { height, overflow: "auto", padding: 8 } },
      attempts.map((attempt) => React.createElement(Attempt, attempt))
    );
  }

  return {
    setters: [
      function (module) { React = module.default ?? module; },
      function (module) { PanelPlugin = module.PanelPlugin; },
      function (module) { Button = module.Button; useTheme2 = module.useTheme2; },
    ],
    execute: function () {
      exports("plugin", new PanelPlugin(JSONPanel));
    },
  };
});
