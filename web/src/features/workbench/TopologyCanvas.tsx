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
  useStore,
  type NodeProps,
  type OnConnect,
} from "@xyflow/react";
import { Box, Monitor, Network, Router } from "lucide-react";
import { useComputedColorScheme } from "@mantine/core";
import type { Edge } from "@xyflow/react";
import type { Schema } from "../../api/client";
import { stateLabel } from "../../foundation/format";
import { topologyNodeSize, type TopologyNode } from "./topology";
import styles from "./TopologyCanvas.module.css";

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
      className={styles.canvas}
      nodes={nodes}
      edges={edges}
      onNodesChange={onNodesChange}
      onEdgesChange={onEdgesChange}
      nodeTypes={nodeTypes}
      onConnect={onConnect}
      isValidConnection={(connection) => {
        const source = nodes.find((node) => node.id === connection.source);
        const target = nodes.find((node) => node.id === connection.target);
        return Boolean(
          source &&
          target &&
          source.type !== target.type &&
          source.data.kind !== "external" &&
          target.data.kind !== "external",
        );
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
  const width = useStore((state) => state.width);
  const height = useStore((state) => state.height);
  useEffect(() => {
    if (!selection) return;
    const frame = requestAnimationFrame(() => {
      const node = getNode(selection);
      if (node)
        void setCenter(
          node.position.x + topologyNodeSize[node.type!].width / 2,
          node.position.y + topologyNodeSize[node.type!].height / 2,
          { zoom: Math.min(getViewport().zoom, 1) },
        );
    });
    return () => cancelAnimationFrame(frame);
  }, [selection, getNode, setCenter, getViewport, width, height]);
  return null;
}

function AssetNode({ data, selected }: NodeProps<TopologyNode>) {
  return (
    <div
      className={styles.asset}
      data-selected={selected}
      style={{ "--network-color": data.color } as React.CSSProperties}
    >
      <Handle
        type="target"
        position={Position.Top}
        isConnectable={data.kind !== "external"}
      />
      <span className={styles.glyph}>
        {data.kind === "external" ? (
          <Router size={20} />
        ) : data.kind === "vm" ? (
          <Monitor size={20} />
        ) : (
          <Box size={20} />
        )}
      </span>
      <div className={styles.assetIdentity}>
        <strong>{data.label}</strong>
        <span className={styles.address}>
          {data.secondary || (data.kind === "vm" ? "虚拟机" : "容器")}
        </span>
      </div>
      {data.kind !== "external" && (
        <span
          className={styles.light}
          data-state={data.state}
          title={stateLabel(data.state)}
        />
      )}
      {data.kind !== "external" && (
        <Handle type="source" position={Position.Bottom} />
      )}
    </div>
  );
}
function NetworkNode({ data, selected }: NodeProps<TopologyNode>) {
  return (
    <div
      className={styles.network}
      data-selected={selected}
      style={{ "--network-color": data.color } as React.CSSProperties}
    >
      <Handle type="target" position={Position.Top} />
      <div className={styles.networkHeading}>
        <Network size={17} />
        <strong>{data.label}</strong>
        <span>{data.count}</span>
      </div>
      <span className={styles.cidr}>{data.secondary}</span>
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
}
