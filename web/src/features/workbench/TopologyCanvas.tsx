import { useEffect } from "react";
import {
  Background,
  BackgroundVariant,
  Controls,
  Handle,
  MiniMap,
  Position,
  ReactFlow,
  useEdgesState,
  useNodesState,
  useReactFlow,
  type NodeProps,
  type OnConnect,
} from "@xyflow/react";
import { Box, Monitor, Network } from "lucide-react";
import { useComputedColorScheme } from "@mantine/core";
import type { Edge } from "@xyflow/react";
import type { Schema } from "../../api/client";
import { stateLabel } from "../../foundation/format";
import type { TopologyNode } from "./topology";

const nodeTypes = { asset: AssetNode, network: NetworkNode };

export function TopologyCanvas({
  nodes: initialNodes,
  edges: initialEdges,
  selection,
  onSelect,
  onPosition,
  editing,
  onConnect,
  onContext,
}: {
  nodes: TopologyNode[];
  edges: Edge[];
  selection?: string;
  onSelect: (id?: string) => void;
  onPosition: (
    positions: NonNullable<Schema<"CanvasView">["positions"]>,
  ) => void;
  editing: boolean;
  onConnect: OnConnect;
  onContext: (id: string, x: number, y: number) => void;
}) {
  const [nodes, setNodes, onNodesChange] =
    useNodesState<TopologyNode>(initialNodes);
  const [edges, setEdges, onEdgesChange] = useEdgesState(initialEdges);
  const scheme = useComputedColorScheme("light");
  useEffect(
    () =>
      setNodes(
        initialNodes.map((node) => ({
          ...node,
          selected: node.id === selection,
        })),
      ),
    [initialNodes, selection, setNodes],
  );
  useEffect(
    () =>
      setEdges(
        initialEdges.map((edge) => ({
          ...edge,
          selected: edge.target === selection || edge.source === selection,
          style: {
            ...edge.style,
            opacity:
              selection &&
              edge.target !== selection &&
              edge.source !== selection
                ? 0.24
                : 1,
            strokeWidth:
              edge.target === selection || edge.source === selection ? 2 : 1.5,
          },
        })),
      ),
    [initialEdges, selection, setEdges],
  );
  return (
    <ReactFlow<TopologyNode>
      nodes={nodes}
      edges={edges}
      onNodesChange={onNodesChange}
      onEdgesChange={onEdgesChange}
      nodeTypes={nodeTypes}
      onConnect={onConnect}
      isValidConnection={(connection) => {
        const source = nodes.find((node) => node.id === connection.source);
        const target = nodes.find((node) => node.id === connection.target);
        return Boolean(source && target && source.type !== target.type);
      }}
      nodesConnectable={editing}
      edgesReconnectable={editing}
      deleteKeyCode={null}
      fitView
      fitViewOptions={{ padding: 0.3, maxZoom: 1 }}
      minZoom={0.18}
      maxZoom={1.8}
      colorMode={scheme}
      onNodeClick={(_, node) => onSelect(node.id)}
      onPaneClick={() => onSelect(undefined)}
      onNodeContextMenu={(event, node) => {
        event.preventDefault();
        onContext(node.id, event.clientX, event.clientY);
      }}
      onNodeDragStop={() =>
        onPosition(
          Object.fromEntries(nodes.map((node) => [node.id, node.position])),
        )
      }
      proOptions={{ hideAttribution: true }}
      ariaLabelConfig={{
        "controls.zoomIn.ariaLabel": "放大画布",
        "controls.zoomOut.ariaLabel": "缩小画布",
        "controls.fitView.ariaLabel": "适应画布",
        "controls.interactive.ariaLabel": "切换交互",
      }}
    >
      <FocusSelection selection={selection} />
      <Background variant={BackgroundVariant.Dots} gap={24} size={1} />
      <Controls showInteractive={false} position="bottom-left" />
      <MiniMap
        pannable
        zoomable
        position="bottom-right"
        nodeColor={(node) => (node.data as TopologyNode["data"]).color}
      />
    </ReactFlow>
  );
}

function FocusSelection({ selection }: { selection?: string }) {
  const { getNode, setCenter, getViewport } = useReactFlow<TopologyNode>();
  useEffect(() => {
    if (!selection) return;
    const frame = requestAnimationFrame(() => {
      const node = getNode(selection);
      if (node)
        void setCenter(node.position.x + 94, node.position.y + 50, {
          zoom: Math.min(getViewport().zoom, 1),
        });
    });
    return () => cancelAnimationFrame(frame);
  }, [selection, getNode, setCenter, getViewport]);
  return null;
}

function AssetNode({ data, selected }: NodeProps<TopologyNode>) {
  return (
    <div
      className={`topology-asset ${selected ? "is-selected" : ""}`}
      style={{ "--network-color": data.color } as React.CSSProperties}
    >
      <Handle type="target" position={Position.Top} />
      <div className="topology-asset-header">
        <span className={`asset-glyph ${data.kind}`}>
          {data.kind === "vm" ? <Monitor size={22} /> : <Box size={22} />}
        </span>
        <span
          className={`node-light ${data.state}`}
          title={stateLabel(data.state)}
        />
      </div>
      <strong>{data.label}</strong>
      <span className="node-address">
        {data.secondary || (data.kind === "vm" ? "虚拟机" : "容器")}
      </span>
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
}
function NetworkNode({ data, selected }: NodeProps<TopologyNode>) {
  return (
    <div
      className={`topology-network ${selected ? "is-selected" : ""}`}
      style={{ "--network-color": data.color } as React.CSSProperties}
    >
      <Handle type="target" position={Position.Top} />
      <div className="network-line">
        <Network size={17} />
        <strong>{data.label}</strong>
        <span>{data.count}</span>
      </div>
      <span className="network-cidr">{data.secondary}</span>
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
}
